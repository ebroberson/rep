package maintain

import (
	"errors"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
)

type Maintainer struct {
	Config
	executorClient executor.Client
	bbs            bbs.RepBBS
	logger         lager.Logger
	clock          clock.Clock
}

type Config struct {
	CellID          string
	RepAddress      string
	Zone            string
	RetryInterval   time.Duration
	RootFSProviders []string
}

func New(
	config Config,
	executorClient executor.Client,
	bbs bbs.RepBBS,
	logger lager.Logger,
	clock clock.Clock,
) *Maintainer {
	return &Maintainer{
		Config:         config,
		executorClient: executorClient,
		bbs:            bbs,
		logger:         logger.Session("maintainer"),
		clock:          clock,
	}
}

const ExecutorPollInterval = time.Second

var ErrSignaledWhileWaiting = errors.New("signaled while waiting for executor")

func (m *Maintainer) Run(sigChan <-chan os.Signal, ready chan<- struct{}) error {
	m.logger.Info("starting-executor-heartbeat")
	defer m.logger.Info("complete-executor-heartbeat")
	for {
		heartbeater, err := m.waitForExecutor(sigChan)
		if err != nil {
			m.logger.Error("error-while-waiting-for-executor", err)
			return err
		}

		err = m.heartbeat(sigChan, ready, heartbeater)
		ready = nil
		if err == nil {
			return nil
		}

		m.logger.Error("executor-ping-failed", err)
	}
}

func (m *Maintainer) waitForExecutor(sigChan <-chan os.Signal) (ifrit.Runner, error) {
	m.logger.Info("start-waiting-for-executor")
	defer m.logger.Info("complete-waiting-for-executor")

	sleeper := m.clock.NewTimer(ExecutorPollInterval)
	for {
		m.logger.Debug("waiting-pinging-executor")
		err := m.executorClient.Ping()
		if err == nil {
			return m.createHeartbeater()
		}

		m.logger.Error("failed-to-ping-executor-on-start", err)

		sleeper.Reset(ExecutorPollInterval)
		select {
		case <-sigChan:
			m.logger.Info("signaled-while-waiting-for-executor")
			return nil, ErrSignaledWhileWaiting
		case <-sleeper.C():
		}
	}
}

func (m *Maintainer) createHeartbeater() (ifrit.Runner, error) {
	resources, err := m.executorClient.TotalResources()
	if err != nil {
		return nil, err
	}

	cellCapacity := models.NewCellCapacity(resources.MemoryMB, resources.DiskMB, resources.Containers)
	cellPresence := models.NewCellPresence(m.CellID, m.RepAddress, m.Zone, cellCapacity, m.RootFSProviders...)
	return m.bbs.NewCellPresence(cellPresence, m.RetryInterval), nil
}

func (m *Maintainer) heartbeat(sigChan <-chan os.Signal, ready chan<- struct{}, heartbeater ifrit.Runner) error {
	m.logger.Info("start-heartbeating")
	defer m.logger.Info("complete-heartbeating")
	ticker := m.clock.NewTicker(m.RetryInterval)
	defer ticker.Stop()

	heartbeatProcess := ifrit.Background(heartbeater)
	heartbeatExitChan := heartbeatProcess.Wait()
	select {
	case <-heartbeatProcess.Ready():
		m.logger.Info("ready")
	case err := <-heartbeatExitChan:
		if err != nil {
			m.logger.Error("heartbeat-exited", err)
		}
		return err
	case <-sigChan:
		m.logger.Info("signaled-while-starting-heatbeater")
		heartbeatProcess.Signal(os.Kill)
		<-heartbeatExitChan
		return nil
	}

	if ready != nil {
		close(ready)
	}

	for {
		select {
		case err := <-heartbeatExitChan:
			m.logger.Error("heartbeat-lost-lock", err)
			return err

		case <-sigChan:
			m.logger.Info("signaled-while-heartbeating")
			heartbeatProcess.Signal(os.Kill)
			<-heartbeatExitChan
			return nil

		case <-ticker.C():
			m.logger.Debug("heartbeat-pinging-executor")
			err := m.executorClient.Ping()
			if err == nil {
				continue
			}

			m.logger.Info("start-signaling-heartbeat-to-stop")
			heartbeatProcess.Signal(os.Kill)
			select {
			case <-heartbeatExitChan:
				m.logger.Info("heartbeat-stopped")
				return err
			case <-sigChan:
				m.logger.Info("signaled-while-waiting-for-heartbeat-to-stop")
				return nil
			}
		}
	}
}
