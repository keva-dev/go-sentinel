package sentinel

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

func (m *masterInstance) killed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isKilled

}

func (s *Sentinel) masterPingRoutine(m *masterInstance) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !m.killed() {
		<-ticker.C
		_, err := m.client.Ping()
		if err != nil {
			if time.Since(m.lastSuccessfulPing) > m.sentinelConf.DownAfter {
				//notify if it is down
				if m.getState() == masterStateUp {
					m.mu.Lock()
					m.state = masterStateSubjDown
					m.downSince = time.Now()
					s.logger.Warnf("master %s is subjectively down", m.name)
					m.mu.Unlock()
					select {
					case m.subjDownNotify <- struct{}{}:
					default:
					}
				}
			}
			continue
		} else {
			m.mu.Lock()
			state := m.state
			m.lastSuccessfulPing = time.Now()
			m.mu.Unlock()
			if state == masterStateSubjDown || state == masterStateObjDown {
				m.mu.Lock()
				m.state = masterStateUp
				m.mu.Unlock()
				//select {
				//case m.reviveNotify <- struct{}{}:
				//default:
				//}
				//revive
			}

		}

	}
}

func (m *masterInstance) getFailOverStartTime() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failOverStartTime
}

func (m *masterInstance) getFailOverEpoch() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failOverEpoch
}

func (m *masterInstance) getFailOverState() failOverState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failOverState
}
func (m *masterInstance) getAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("%s:%s", m.host, m.port)
}

func (m *masterInstance) getState() masterInstanceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// for now, only say hello through master, redis also say hello through slaves
func (s *Sentinel) sayHelloRoutine(m *masterInstance, helloChan HelloChan) {
	for !m.killed() {
		time.Sleep(1 * time.Second)
		m.mu.Lock()
		masterName := m.name
		masterIP := m.host
		masterPort := m.port
		masterConfigEpoch := m.configEpoch
		m.mu.Unlock()
		info := fmt.Sprintf("%s,%s,%s,%d,%s,%s,%s,%d",
			s.conf.Binds[0], s.conf.Port, s.selfID(),
			s.getCurrentEpoch(), masterName, masterIP, masterPort, masterConfigEpoch,
		)
		err := helloChan.Publish(info)
		if err != nil {
			s.logger.Errorf("helloChan.Publish: %s", err)
		}
	}
}

func (s *Sentinel) subscribeHello(m *masterInstance) {
	helloChan := m.client.SubscribeHelloChan()

	go s.sayHelloRoutine(m, helloChan)
	for !m.killed() {
		newmsg, err := helloChan.Receive()
		if err != nil {
			s.logger.Errorf("helloChan.Receive: %s", err)
			continue
			//skip for now
		}
		parts := strings.Split(newmsg, ",")
		if len(parts) != 8 {
			s.logger.Errorf("helloChan.Receive: invalid format for newmsg: %s", newmsg)
			continue
		}
		runid := parts[2]
		m.mu.Lock()
		_, ok := m.sentinels[runid]
		if !ok {
			client, err := newRPCClient(parts[0], parts[1])
			if err != nil {
				s.logger.Errorf("newRPCClient: cannot create new client to other sentinel with info: %s: %s", newmsg, err)
				m.mu.Unlock()
				continue
			}
			si := &sentinelInstance{
				mu:     sync.Mutex{},
				sdown:  false,
				client: client,
				runID:  runid,
			}

			m.sentinels[runid] = si
			s.logger.Infof("subscribeHello: connected to new sentinel: %s", newmsg)
		}
		m.mu.Unlock()
		//ignore if exist for now
	}
}

func (s *Sentinel) masterRoutine(m *masterInstance) {

	go s.masterPingRoutine(m)
	go s.subscribeHello(m)
	infoTicker := time.NewTicker(10 * time.Second)
	for !m.killed() {
		switch m.getState() {
		case masterStateUp:
			select {
			case <-m.shutdownChan:
				return
			case <-m.subjDownNotify:
				//notify allowing break select loop early, rather than waiting for 10s
				infoTicker.Stop()

			case <-infoTicker.C:
				info, err := m.client.Info()
				if err != nil {
					//TODO continue for now
					continue
				}
				roleSwitched, err := s.parseInfoMaster(m.getAddr(), info)
				if err != nil {
					s.logger.Errorf("parseInfoMaster: %v", err)
					continue
					// continue for now
				}
				if roleSwitched {
					panic("unimlemented")
					//TODO
					// s.parseInfoSlave()
				}
			}

		case masterStateSubjDown:
		SdownLoop:
			for {
				switch m.getState() {
				case masterStateSubjDown:
					// check if quorum as met
					s.askSentinelsIfMasterIsDown(m)
					s.checkObjDown(m)
					if m.getState() == masterStateObjDown {
						s.notifyMasterDownToSlaveRoutines(m)
						break SdownLoop
					}
				case masterStateObjDown:
					panic("no other process should set masterStateObjDown")
				case masterStateUp:
					break SdownLoop
				}
				time.Sleep(1 * time.Second)
			}
		case masterStateObjDown:

			// stopAskingOtherSentinels: used when the fsm state wnats to turn back to this current line
			// canceling the on going goroutine, or when the failover is successful
			ctx, stopAskingOtherSentinels := context.WithCancel(context.Background())
			go s.askOtherSentinelsEach1Sec(ctx, m)
			for m.getState() == masterStateObjDown {
				startFailover := s.checkIfShouldFailover(m)
				if startFailover {
					break
				}
			}
			// If any logic changing state of master to something != masterStateObjDown, this code below will be broken
		failOverFSM:
			for {
				switch m.getFailOverState() {
				case failOverWaitLeaderElection:
					aborted := s.checkElectionStatus(m)
					if aborted {
						stopAskingOtherSentinels()
						break failOverFSM
					}

				case failOverSelectSlave:
					slave := s.selectSlave(m)

					// abort failover, start voting for new epoch
					if slave == nil {
						s.abortFailover(m)
						stopAskingOtherSentinels()
					} else {
						s.promoteSlave(m, slave)
					}
				case failOverPromoteSlave:
					m.mu.Lock()
					promotedSlave := m.promotedSlave
					m.mu.Unlock()
					timeout := time.NewTimer(m.sentinelConf.FailoverTimeout)
					defer timeout.Stop()
					select {
					case <-timeout.C:
						s.abortFailover(m)
						// when new slave recognized to have switched role, it will notify this channel
						// or time out
					case <-promotedSlave.masterRoleSwitchChan:
						m.mu.Lock()
						m.failOverState = failOverReconfSlave
						m.failOverStateChangeTime = time.Now()
						epoch := m.failOverEpoch
						m.mu.Unlock()
						s.logger.Debugw(logEventSlavePromoted,
							"run_id", promotedSlave.runID,
							"epoch", epoch)
					}
				case failOverReconfSlave:
					s.reconfigRemoteSlaves(m)
					s.resetMasterInstance(m)
					time.Sleep(1 * time.Second)
				}
			}
		}
	}
}
func (s *Sentinel) resetMasterInstance(m *masterInstance) {
	m.mu.Lock()
	promoted := m.promotedSlave
	name := m.name
	m.isKilled = true
	m.mu.Unlock()

	promoted.mu.Lock()
	promotedAddr := promoted.addr
	host, port := promoted.host, promoted.port
	newRunID := m.promotedSlave.runID
	promoted.mu.Unlock()

	cl, err := s.clientFactory(promotedAddr)
	if err != nil {
		panic("todo")
	}
	newSlaves := map[string]*slaveInstance{}
	newMaster := &masterInstance{
		runID:              newRunID,
		sentinelConf:       m.sentinelConf,
		name:               name,
		host:               host,
		port:               port,
		configEpoch:        m.configEpoch,
		mu:                 sync.Mutex{},
		client:             cl,
		slaves:             map[string]*slaveInstance{},
		sentinels:          map[string]*sentinelInstance{},
		state:              masterStateUp,
		lastSuccessfulPing: time.Now(),
		subjDownNotify:     make(chan struct{}),
	}
	m.mu.Lock()
	for _, sl := range m.slaves {
		sl.mu.Lock()

		// kill all running slaves routine, for simplicity
		sl.killed = true
		if sl.addr != promotedAddr {
			newSlaves[sl.addr] = &slaveInstance{
				masterHost:       host,
				masterPort:       port,
				host:             sl.host,
				port:             sl.port,
				addr:             sl.addr,
				replOffset:       sl.replOffset,
				reportedMaster:   newMaster,
				masterDownNotify: make(chan struct{}, 1),
			}
		}

		sl.mu.Unlock()
	}
	oldAddr := fmt.Sprintf("%s:%s", m.host, m.port)
	// turn dead master into slave as well
	newSlaves[oldAddr] = &slaveInstance{
		masterHost:       host,
		masterPort:       port,
		host:             m.host,
		port:             m.port,
		addr:             oldAddr,
		replOffset:       0, //TODO
		reportedMaster:   newMaster,
		masterDownNotify: make(chan struct{}, 1),
	}
	m.slaves = newSlaves
	m.mu.Unlock()

	s.mu.Lock()
	delete(s.masterInstances, oldAddr)
	s.masterInstances[newMaster.getAddr()] = newMaster
	s.mu.Unlock()

	for idx := range newSlaves {
		go s.slaveRoutine(newSlaves[idx])
	}

	go s.masterRoutine(newMaster)
}

// TODO: write test for this func
func (s *Sentinel) reconfigRemoteSlaves(m *masterInstance) error {
	m.mu.Lock()

	slaveList := make([]*slaveInstance, 0, len(m.slaves))
	for runID := range m.slaves {
		slaveList = append(slaveList, m.slaves[runID])
	}
	m.mu.Unlock()
	sema := semaphore.NewWeighted(int64(m.sentinelConf.ParallelSync))

	errgrp := &errgroup.Group{}
	ctx, cancel := context.WithTimeout(context.Background(), m.sentinelConf.ReconfigSlaveTimeout)
	defer cancel()

	for idx := range slaveList {
		slave := slaveList[idx]
		if slave.runID == m.promotedSlave.runID {
			continue
		}
		errgrp.Go(func() error {
			var done bool
			for {
				slave.mu.Lock()
				host, port := slave.host, slave.port
				slave.mu.Unlock()
				err := sema.Acquire(ctx, 1)
				if err != nil {
					return err
				}
				// goroutine exit if ctx is canceled
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				err = slave.client.SlaveOf(host, port)
				if err != nil {
					sema.Release(1)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				slave.mu.Lock()
				slave.reconfigFlag |= reconfigSent
				slave.mu.Unlock()
				break
			}

			for {
				slave.mu.Lock()
				if slave.reconfigFlag&reconfigDone != 0 {
					slave.reconfigFlag = 0
					done = true
				}
				slave.mu.Unlock()
				if done {
					sema.Release(1)
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				// TODO: may use sync.Cond instead of ticking
				time.Sleep(1 * time.Second)
			}
		})
	}
	return errgrp.Wait()
	// update state no matter what
}

func (s *Sentinel) checkIfShouldFailover(m *masterInstance) (startFailover bool) {
	// this code only has to wait in case failover timeout reached and block until the next failover is allowed to
	// continue (2* failover timeout)
	secondsLeft := 2*m.sentinelConf.FailoverTimeout - time.Since(m.getFailOverStartTime())
	if secondsLeft <= 0 {
		locked(s.mu, func() {
			s.currentEpoch += 1
			locked(&m.mu, func() {
				if m.failOverState != failOverWaitLeaderElection {
					s.logger.Debugw(logEventFailoverStateChanged,
						"new_state", failOverWaitLeaderElection,
						"epoch", s.currentEpoch,
					)
				}
				m.failOverState = failOverWaitLeaderElection
				m.failOverStartTime = time.Now()
				m.failOverEpoch = s.currentEpoch
			})
		})
		// mostly, when obj down is met, multiples sentinels will try to send request for vote to be leader
		// to prevent split vote, sleep for a random small duration
		random := rand.Intn(SENTINEL_MAX_DESYNC) * int(time.Millisecond)
		time.Sleep(time.Duration(random))
		return true
	}
	<-time.NewTimer(secondsLeft).C
	return false
}

func (s *Sentinel) checkElectionStatus(m *masterInstance) (aborted bool) {
	leader, epoch := s.checkWhoIsLeader(m)
	isLeader := leader != "" && leader == s.selfID()

	if !isLeader {
		time.Sleep(1 * time.Second)

		//abort if failover take too long
		if time.Since(m.getFailOverStartTime()) > m.sentinelConf.FailoverTimeout {
			s.abortFailover(m)
			return true
		}
		return false
	}

	// do not call cancel(), keep receiving update from other sentinel
	s.logger.Debugw(logEventBecameTermLeader,
		"run_id", leader,
		"epoch", epoch)

	m.mu.Lock()
	m.failOverState = failOverSelectSlave
	m.mu.Unlock()

	s.logger.Debugw(logEventFailoverStateChanged,
		"new_state", failOverSelectSlave,
		"epoch", epoch, // epoch = failover epoch = sentinel current epoch
	)
	return true
}

func (s *Sentinel) promoteSlave(m *masterInstance, slave *slaveInstance) {
	m.mu.Lock()
	m.promotedSlave = slave
	m.failOverState = failOverPromoteSlave
	m.failOverStateChangeTime = time.Now()
	epoch := m.failOverEpoch
	m.mu.Unlock()
	slave.mu.Lock()
	slaveAddr := fmt.Sprintf("%s:%s", slave.host, slave.port)
	slaveID := slave.runID
	s.logger.Debugw(logEventSelectedSlave,
		"slave_addr", slaveAddr,
		"slave_id", slaveID,
		"epoch", epoch,
	)

	s.logger.Debugw(logEventFailoverStateChanged,
		"new_state", failOverPromoteSlave,
		"epoch", epoch,
	)
}
func (s *Sentinel) abortFailover(m *masterInstance) {
	m.mu.Lock()
	m.failOverState = failOverNone
	m.failOverStateChangeTime = time.Now()
	epoch := m.failOverEpoch
	m.mu.Unlock()

	s.logger.Debugw(logEventFailoverStateChanged,
		"new_state", failOverNone,
		"epoch", epoch,
	)
}

func (s *Sentinel) notifyMasterDownToSlaveRoutines(m *masterInstance) {
	m.mu.Lock()
	for idx := range m.slaves {
		m.slaves[idx].masterDownNotify <- struct{}{}
	}
	m.mu.Unlock()
}

func (s *Sentinel) checkWhoIsLeader(m *masterInstance) (string, int) {
	instancesVotes := map[string]int{}
	currentEpoch := s.getCurrentEpoch()
	// instancesVoters := map[string][]string{}
	m.mu.Lock()
	for _, sen := range m.sentinels {
		sen.mu.Lock()
		if sen.leaderID != "" && sen.leaderEpoch == currentEpoch {
			instancesVotes[sen.leaderID] = instancesVotes[sen.leaderID] + 1
			// instancesVoters[sen.leaderID] = append(instancesVoters[sen.leaderID], sen.runID)
		}
		sen.mu.Unlock()
	}
	totalSentinels := len(m.sentinels) + 1
	m.mu.Unlock()
	maxVote := 0
	var winner string
	for runID, vote := range instancesVotes {
		if vote > maxVote {
			maxVote = vote
			winner = runID
		}
	}
	epoch := m.getFailOverEpoch()
	var leaderEpoch int
	var votedByMe string
	if winner != "" {
		// vote for the most winning candidate
		leaderEpoch, votedByMe = s.voteLeader(m, epoch, winner)
	} else {
		leaderEpoch, votedByMe = s.voteLeader(m, epoch, s.selfID())
	}
	if votedByMe != "" && leaderEpoch == epoch {
		instancesVotes[votedByMe] = instancesVotes[votedByMe] + 1
		if instancesVotes[votedByMe] > maxVote {
			maxVote = instancesVotes[votedByMe]
			winner = votedByMe
		}
		// instancesVoters[votedByMe] = append(instancesVoters[votedByMe], s.selfID())
	}
	quorum := totalSentinels/2 + 1
	if winner != "" && (maxVote < quorum || maxVote < m.sentinelConf.Quorum) {
		winner = ""
	}

	return winner, currentEpoch
}

func (s *Sentinel) checkObjDown(m *masterInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	quorum := m.sentinelConf.Quorum
	if len(m.sentinels)+1 < quorum {
		panic(fmt.Sprintf("quorum too large (%d) or too few sentinel instances (%d)", quorum, len(m.sentinels)))
		// TODO: log warning only
	}

	down := 1
	for _, sentinel := range m.sentinels {
		sentinel.mu.Lock()
		if sentinel.sdown {
			down++
		}
		sentinel.mu.Unlock()
	}
	if down >= quorum {
		m.state = masterStateObjDown
	}
}

func (s *Sentinel) askOtherSentinelsEach1Sec(ctx context.Context, m *masterInstance) {
	m.mu.Lock()
	masterName := m.name
	masterIp := m.host
	masterPort := m.port

	for idx := range m.sentinels {
		sentinel := m.sentinels[idx]
		go func() {
			for m.getState() >= masterStateSubjDown {
				select {
				case <-ctx.Done():
					return
				default:
				}

				s.mu.Lock()
				currentEpoch := s.currentEpoch //epoch may change during failover
				selfID := s.runID              // locked as well for sure
				s.mu.Unlock()

				// do not ask for vote if has not started failover
				if m.getFailOverState() == failOverNone {
					selfID = ""
				}
				reply, err := sentinel.client.IsMasterDownByAddr(IsMasterDownByAddrArgs{
					Name:         masterName,
					IP:           masterIp,
					Port:         masterPort,
					CurrentEpoch: currentEpoch,
					SelfID:       selfID,
				})
				if err != nil {
					s.logger.Errorf("sentinel.client.IsMasterDownByAddr: %s", err)
					//skip for now
				} else {
					sentinel.mu.Lock()
					sentinel.sdown = reply.MasterDown

					if reply.VotedLeaderID != "" {
						s.logger.Debugw(logEventNeighborVotedFor,
							"neighbor_id", sentinel.runID,
							"voted_for", reply.VotedLeaderID,
							"epoch", reply.LeaderEpoch,
						)
						sentinel.leaderEpoch = reply.LeaderEpoch
						sentinel.leaderID = reply.VotedLeaderID
					}

					sentinel.mu.Unlock()
				}
				time.Sleep(1 * time.Second)
			}

		}()
	}
	m.mu.Unlock()

}

func (s *Sentinel) askSentinelsIfMasterIsDown(m *masterInstance) {
	s.mu.Lock()
	currentEpoch := s.currentEpoch
	s.mu.Unlock()

	m.mu.Lock()
	masterName := m.name
	masterIp := m.host
	masterPort := m.port

	for _, sentinel := range m.sentinels {
		go func(sInstance *sentinelInstance) {
			sInstance.mu.Lock()
			lastReply := sInstance.lastMasterDownReply
			sInstance.mu.Unlock()
			if time.Since(lastReply) < 1*time.Second {
				return
			}
			if m.getState() == masterStateSubjDown {
				reply, err := sInstance.client.IsMasterDownByAddr(IsMasterDownByAddrArgs{
					Name:         masterName,
					IP:           masterIp,
					Port:         masterPort,
					CurrentEpoch: currentEpoch,
					SelfID:       "",
				})
				if err != nil {
					//skip for now
				} else {
					sInstance.mu.Lock()
					sInstance.lastMasterDownReply = time.Now()
					sInstance.sdown = reply.MasterDown
					sInstance.mu.Unlock()
				}
			} else {
				return
			}

		}(sentinel)
	}
	m.mu.Unlock()

}

type masterInstance struct {
	sentinelConf  MasterMonitor
	isKilled      bool
	name          string
	mu            sync.Mutex
	runID         string
	slaves        map[string]*slaveInstance
	promotedSlave *slaveInstance
	sentinels     map[string]*sentinelInstance
	host          string
	port          string
	shutdownChan  chan struct{}
	client        InternalClient
	// infoClient   internalClient

	state          masterInstanceState
	subjDownNotify chan struct{}
	downSince      time.Time

	lastSuccessfulPing time.Time

	failOverState           failOverState
	failOverEpoch           int
	failOverStartTime       time.Time
	failOverStateChangeTime time.Time

	leaderEpoch int
	leaderID    string
	// failOverStartTime time.Time

	configEpoch int
}

type failOverState int

var (
	failOverNone               failOverState = 0
	failOverWaitLeaderElection failOverState = 1
	failOverSelectSlave        failOverState = 2
	failOverPromoteSlave       failOverState = 3
	failOverReconfSlave        failOverState = 4
	failOverStateValueMap                    = map[string]failOverState{
		"none":                 failOverNone,
		"wait_leader_election": failOverWaitLeaderElection,
		"select_slave":         failOverSelectSlave,
		"promote_slave":        failOverPromoteSlave,
		"reconfig_slave":       failOverReconfSlave,
	}
	failOverStateMap = map[failOverState]string{
		0: "none",
		1: "wait_leader_election",
		2: "select_slave",
		3: "promote_slave",
		4: "reconfig_slave",
	}
)

func (s failOverState) String() string {
	return failOverStateMap[s]
}
