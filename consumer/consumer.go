package consumer

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mailgun/kafka-pixy/Godeps/_workspace/src/github.com/mailgun/log"
	"github.com/mailgun/kafka-pixy/Godeps/_workspace/src/github.com/mailgun/sarama"
	"github.com/mailgun/kafka-pixy/Godeps/_workspace/src/github.com/wvanbergen/kazoo-go"
	"github.com/mailgun/kafka-pixy/config"
)

type (
	ErrSetup          error
	ErrBufferOverflow error
	ErrRequestTimeout error
)

var (
	// If this channel is not `nil` then exclusive consumers will use it to
	// notify when they fetch the very first message.
	firstMessageFetchedCh chan *exclusiveConsumer
)

// SmartConsumer is a Kafka consumer implementation that automatically
// maintains consumer groups registrations and topic subscriptions. Whenever a
// a message from a particular topic is consumed by a particular consumer group
// SmartConsumer checks if it has registered with the consumer group, and
// registers otherwise. Then it checks if it has subscribed for the topic, and
// subscribes otherwise. Later if a particular topic has not been consumed for
// the `Config.Consumer.RegistrationTimeout` period of time, then the consumer
// unsubscribes from the topic, likewise if a consumer group has not seen any
// requests for that period then the consumer deregisters from the group.
type T struct {
	baseCID     *sarama.ContextID
	cfg         *config.T
	dispatcher  *dispatcher
	kafkaClient sarama.Client
	offsetMgr   sarama.OffsetManager
	kazooConn   *kazoo.Kazoo
}

// Spawn creates a consumer instance with the specified configuration and
// starts all its goroutines.
func Spawn(cfg *config.T) (*T, error) {
	saramaCfg := sarama.NewConfig()
	saramaCfg.ClientID = cfg.ClientID
	saramaCfg.ChannelBufferSize = cfg.Consumer.ChannelBufferSize
	saramaCfg.Consumer.Offsets.CommitInterval = 50 * time.Millisecond
	saramaCfg.Consumer.Retry.Backoff = cfg.Consumer.BackOffTimeout
	saramaCfg.Consumer.Fetch.Default = 1024 * 1024

	kafkaClient, err := sarama.NewClient(cfg.Kafka.SeedPeers, saramaCfg)
	if err != nil {
		return nil, ErrSetup(fmt.Errorf("failed to create sarama.Client: err=(%v)", err))
	}
	offsetMgr, err := sarama.NewOffsetManagerFromClient(kafkaClient)
	if err != nil {
		return nil, ErrSetup(fmt.Errorf("failed to create sarama.OffsetManager: err=(%v)", err))
	}

	kazooCfg := kazoo.NewConfig()
	kazooCfg.Chroot = cfg.ZooKeeper.Chroot
	// ZooKeeper documentation says following about the session timeout: "The
	// current (ZooKeeper) implementation requires that the timeout be a
	// minimum of 2 times the tickTime (as set in the server configuration) and
	// a maximum of 20 times the tickTime". The default tickTime is 2 seconds.
	// See http://zookeeper.apache.org/doc/trunk/zookeeperProgrammers.html#ch_zkSessions
	kazooCfg.Timeout = 15 * time.Second

	kazooConn, err := kazoo.NewKazoo(cfg.ZooKeeper.SeedPeers, kazooCfg)
	if err != nil {
		return nil, ErrSetup(fmt.Errorf("failed to create kazoo.Kazoo: err=(%v)", err))
	}

	sc := &T{
		baseCID:     sarama.RootCID.NewChild("smartConsumer"),
		cfg:         cfg,
		kafkaClient: kafkaClient,
		offsetMgr:   offsetMgr,
		kazooConn:   kazooConn,
	}
	sc.dispatcher = newDispatcher(sc.baseCID, sc, sc.cfg)
	sc.dispatcher.start()
	return sc, nil
}

// Stop sends the shutdown signal to all internal goroutines and blocks until
// all of them are stopped. It is guaranteed that all last consumed offsets of
// all consumer groups/topics are committed to Kafka before SmartConsumer stops.
func (sc *T) Stop() {
	sc.dispatcher.stop()
}

// Consume consumes a message from the specified topic on behalf of the
// specified consumer group. If there are no more new messages in the topic
// at the time of the request then it will block for
// `Config.Consumer.LongPollingTimeout`. If no new message is produced during
// that time, then `ErrConsumerRequestTimeout` is returned.
//
// Note that during state transitions topic subscribe<->unsubscribe and
// consumer group register<->deregister the method may return either
// `ErrConsumerBufferOverflow` or `ErrConsumerRequestTimeout` even when there
// are messages available for consumption. In that case the user should back
// off a bit and then repeat the request.
func (sc *T) Consume(group, topic string) (*sarama.ConsumerMessage, error) {
	replyCh := make(chan consumeResult, 1)
	sc.dispatcher.requests() <- consumeRequest{time.Now().UTC(), group, topic, replyCh}
	result := <-replyCh
	return result.Msg, result.Err
}

type consumeRequest struct {
	timestamp time.Time
	group     string
	topic     string
	replyCh   chan<- consumeResult
}

type consumeResult struct {
	Msg *sarama.ConsumerMessage
	Err error
}

func (sc *T) String() string {
	return sc.baseCID.String()
}

func (sc *T) dispatchKey(req consumeRequest) string {
	return req.group
}

func (sc *T) newDispatchTier(key string) dispatchTier {
	return sc.newConsumerGroup(key)
}

// groupConsumer manages a fleet of topic consumers and disposes of those that
// have been inactive for the `Config.Consumer.DisposeAfter` period of time.
type groupConsumer struct {
	baseCID               *sarama.ContextID
	cfg                   *config.T
	group                 string
	dispatcher            *dispatcher
	kafkaClient           sarama.Client
	dumbConsumer          sarama.Consumer
	offsetMgr             sarama.OffsetManager
	kazooConn             *kazoo.Kazoo
	registry              *consumerGroupRegistry
	topicGears            map[string]*topicGear
	addTopicConsumerCh    chan *topicConsumer
	deleteTopicConsumerCh chan *topicConsumer
	stoppingCh            chan none
	wg                    sync.WaitGroup
}

func (sc *T) newConsumerGroup(group string) *groupConsumer {
	gc := &groupConsumer{
		baseCID:               sc.baseCID.NewChild(group),
		cfg:                   sc.cfg,
		group:                 group,
		kafkaClient:           sc.kafkaClient,
		offsetMgr:             sc.offsetMgr,
		kazooConn:             sc.kazooConn,
		topicGears:            make(map[string]*topicGear),
		addTopicConsumerCh:    make(chan *topicConsumer),
		deleteTopicConsumerCh: make(chan *topicConsumer),
		stoppingCh:            make(chan none),
	}
	gc.dispatcher = newDispatcher(gc.baseCID, gc, sc.cfg)
	return gc
}

func (gc *groupConsumer) String() string {
	return gc.baseCID.String()
}

func (gc *groupConsumer) addTopicConsumer() chan<- *topicConsumer {
	return gc.addTopicConsumerCh
}

func (gc *groupConsumer) deleteTopicConsumer() chan<- *topicConsumer {
	return gc.deleteTopicConsumerCh
}

func (gc *groupConsumer) key() string {
	return gc.group
}

func (gc *groupConsumer) start(stoppedCh chan<- dispatchTier) {
	spawn(&gc.wg, func() {
		defer func() { stoppedCh <- gc }()
		var err error
		gc.dumbConsumer, err = sarama.NewConsumerFromClient(gc.kafkaClient)
		if err != nil {
			// Must never happen.
			panic(ErrSetup(fmt.Errorf("failed to create sarama.Consumer: err=(%v)", err)))
		}
		gc.registry = spawnConsumerGroupRegister(gc.group, gc.cfg.ClientID, gc.cfg, gc.kazooConn)
		var manageWg sync.WaitGroup
		spawn(&manageWg, gc.managePartitions)
		gc.dispatcher.start()
		// Wait for a stop signal and shutdown gracefully when one is received.
		<-gc.stoppingCh
		gc.dispatcher.stop()
		gc.registry.stop()
		manageWg.Wait()
		gc.dumbConsumer.Close()
	})
}

func (gc *groupConsumer) requests() chan<- consumeRequest {
	return gc.dispatcher.requests()
}

func (gc *groupConsumer) stop() {
	close(gc.stoppingCh)
	gc.wg.Wait()
}

func (gc *groupConsumer) dispatchKey(req consumeRequest) string {
	return req.topic
}

func (gc *groupConsumer) newDispatchTier(key string) dispatchTier {
	tc := gc.newTopicConsumer(key)
	return tc
}

func (gc *groupConsumer) managePartitions() {
	cid := gc.baseCID.NewChild("managePartitions")
	defer cid.LogScope()()
	var (
		topicConsumers                = make(map[string]*topicConsumer)
		topics                        []string
		memberSubscriptions           map[string][]string
		ok                            = true
		nilOrRetryCh                  <-chan time.Time
		nilOrRegistryTopicsCh         chan<- []string
		shouldRebalance, canRebalance = false, true
		rebalanceResultCh             = make(chan error, 1)
	)
	for {
		select {
		case tc := <-gc.addTopicConsumerCh:
			topicConsumers[tc.topic] = tc
			topics = listTopics(topicConsumers)
			nilOrRegistryTopicsCh = gc.registry.topics()
			continue
		case tc := <-gc.deleteTopicConsumerCh:
			delete(topicConsumers, tc.topic)
			topics = listTopics(topicConsumers)
			nilOrRegistryTopicsCh = gc.registry.topics()
			continue
		case nilOrRegistryTopicsCh <- topics:
			nilOrRegistryTopicsCh = nil
			continue
		case memberSubscriptions, ok = <-gc.registry.membershipChanges():
			if !ok {
				goto done
			}
			nilOrRetryCh = nil
			shouldRebalance = true
		case err := <-rebalanceResultCh:
			canRebalance = true
			if err != nil {
				log.Errorf("<%s> rebalance failed: err=(%s)", cid, err)
				nilOrRetryCh = time.After(gc.cfg.Consumer.BackOffTimeout)
				continue
			}
		case <-nilOrRetryCh:
			shouldRebalance = true
		}

		if shouldRebalance && canRebalance {
			// Copy topicConsumers to make sure `rebalance` doesn't see any
			// changes we make while it is running.
			topicConsumerCopy := make(map[string]*topicConsumer, len(topicConsumers))
			for topic, tc := range topicConsumers {
				topicConsumerCopy[topic] = tc
			}
			go gc.rebalance(topicConsumerCopy, memberSubscriptions, rebalanceResultCh)
			shouldRebalance, canRebalance = false, false
		}
	}
done:
	var wg sync.WaitGroup
	for _, tg := range gc.topicGears {
		tg := tg
		spawn(&wg, func() { gc.rewireMultiplexer(tg, nil) })
	}
	wg.Wait()
}

func (gc *groupConsumer) rebalance(topicConsumers map[string]*topicConsumer,
	memberSubscriptions map[string][]string, rebalanceResultCh chan<- error,
) {
	cid := gc.baseCID.NewChild("rebalance")
	defer cid.LogScope(topicConsumers, memberSubscriptions)()

	assignedPartitions, err := gc.resolvePartitions(memberSubscriptions)
	if err != nil {
		rebalanceResultCh <- err
		return
	}
	log.Infof("<%s> assigned: %v", cid, assignedPartitions)
	var wg sync.WaitGroup
	// Stop consuming partitions that are no longer assigned to this group
	// and start consuming newly assigned partitions for topics that has been
	// consumed already.
	for topic, tg := range gc.topicGears {
		tg := tg
		assignedTopicPartition := assignedPartitions[topic]
		spawn(&wg, func() { gc.rewireMultiplexer(tg, assignedTopicPartition) })
	}
	// Start consuming partitions for topics that has not been consumed before.
	for topic, assignedTopicPartitions := range assignedPartitions {
		tc := topicConsumers[topic]
		tg := gc.topicGears[topic]
		if tc == nil || tg != nil {
			continue
		}
		tg = &topicGear{
			topicConsumer:      tc,
			exclusiveConsumers: make(map[int32]*exclusiveConsumer, len(assignedTopicPartitions)),
		}
		assignedTopicPartitions := assignedTopicPartitions
		spawn(&wg, func() { gc.rewireMultiplexer(tg, assignedTopicPartitions) })
		gc.topicGears[topic] = tg
	}
	wg.Wait()
	// Clean up gears for topics that are not consumed anymore.
	for topic, tg := range gc.topicGears {
		if tg.multiplexer == nil {
			delete(gc.topicGears, topic)
		}
	}
	// Notify the caller that rebalancing has completed successfully.
	rebalanceResultCh <- nil
	return
}

// rewireMultiplexer ensures that only assigned partitions are multiplexed to
// the topic consumer. It stops exclusive consumers for partitions that are not
// assigned anymore, spins up exclusive consumers for newly assigned partitions,
// and restarts the multiplexer to account for the changes if there is any.
func (gc *groupConsumer) rewireMultiplexer(tg *topicGear, assigned map[int32]bool) {
	var wg sync.WaitGroup
	for partition, ec := range tg.exclusiveConsumers {
		if !assigned[partition] {
			if tg.multiplexer != nil {
				tg.multiplexer.stop()
				tg.multiplexer = nil
			}
			spawn(&wg, ec.stop)
			delete(tg.exclusiveConsumers, partition)
		}
	}
	wg.Wait()
	for partition := range assigned {
		if _, ok := tg.exclusiveConsumers[partition]; !ok {
			if tg.multiplexer != nil {
				tg.multiplexer.stop()
				tg.multiplexer = nil
			}
			ec := gc.spawnExclusiveConsumer(tg.topicConsumer.topic, partition)
			tg.exclusiveConsumers[partition] = ec
		}
	}
	if tg.multiplexer == nil && len(tg.exclusiveConsumers) > 0 {
		muxIns := make([]multiplexerIn, 0, len(tg.exclusiveConsumers))
		for _, ec := range tg.exclusiveConsumers {
			muxIns = append(muxIns, ec)
		}
		tg.multiplexer = spawnMultiplexer(tg.topicConsumer.contextID, tg.topicConsumer, muxIns)
	}
}

// resolvePartitions takes a `subscriber->topics` map and returns a
// `topic->partitions` map that for every consumed topic tells what partitions
// this consumer group instance is responsible for.
func (gc *groupConsumer) resolvePartitions(subscribersToTopics map[string][]string) (
	assignedPartitions map[string]map[int32]bool, err error,
) {
	// Convert subscribers->topics to topic->subscribers map.
	topicsToSubscribers := make(map[string][]string)
	for subscriberID, topics := range subscribersToTopics {
		for _, topic := range topics {
			topicsToSubscribers[topic] = append(topicsToSubscribers[topic], subscriberID)
		}
	}
	// Create a set of topics this consumer group member subscribed to.
	subscribedTopics := make(map[string]bool)
	for _, topic := range subscribersToTopics[gc.cfg.ClientID] {
		subscribedTopics[topic] = true
	}
	// Resolve new partition assignments for the subscribed topics.
	assignedPartitions = make(map[string]map[int32]bool)
	for topic := range subscribedTopics {
		topicPartitions, err := gc.kafkaClient.Partitions(topic)
		if err != nil {
			return nil, fmt.Errorf("failed to get partition list: topic=%s, err=(%s)", topic, err)
		}
		partitionsToSubscribers := assignPartitionsToSubscribers(topicPartitions, topicsToSubscribers[topic])
		assignedTopicPartitions := partitionsToSubscribers[gc.cfg.ClientID]
		assignedPartitions[topic] = assignedTopicPartitions
	}
	return assignedPartitions, nil
}

// assignPartitionsToSubscribers does what the name says. The algorithm used
// closely resembles the algorithm implemented by the standard Java High-Level
// consumer (see http://kafka.apache.org/documentation.html#distributionimpl
// and scroll down to *Consumer registration algorithm*) except it does not take
// in account how partitions are distributed among brokers.
func assignPartitionsToSubscribers(partitions []int32, subscribers []string) map[string]map[int32]bool {
	partitionCount := len(partitions)
	subscriberCount := len(subscribers)
	if partitionCount == 0 || subscriberCount == 0 {
		return nil
	}
	sort.Sort(Int32Slice(partitions))
	sort.Sort(sort.StringSlice(subscribers))

	partitionsToSubscribers := make(map[string]map[int32]bool, subscriberCount)
	partitionsPerSubscriber := partitionCount / subscriberCount
	extra := partitionCount - subscriberCount*partitionsPerSubscriber

	begin := 0
	for _, subscriberID := range subscribers {
		end := begin + partitionsPerSubscriber
		if extra != 0 {
			end++
			extra--
		}
		for _, partition := range partitions[begin:end] {
			topicAssignments := partitionsToSubscribers[subscriberID]
			if topicAssignments == nil {
				topicAssignments = make(map[int32]bool, partitionCount)
				partitionsToSubscribers[subscriberID] = topicAssignments
			}
			partitionsToSubscribers[subscriberID][partition] = true
		}
		begin = end
	}
	return partitionsToSubscribers
}

func listTopics(topicConsumers map[string]*topicConsumer) []string {
	topics := make([]string, 0, len(topicConsumers))
	for topic := range topicConsumers {
		topics = append(topics, topic)
	}
	return topics
}

// topicGear represents a set of actors that a consumer group maintains for
// each consumed topic.
type topicGear struct {
	topicConsumer      *topicConsumer
	multiplexer        *multiplexer
	exclusiveConsumers map[int32]*exclusiveConsumer
}

// topicConsumer implements a consumer request dispatch tier responsible for
// consumption of a particular topic by a consumer group. It receives requests
// on the `requests()` channel and replies with messages selected by the
// respective multiplexer. If there has been no message received for
// `Config.Consumer.LongPollingTimeout` then a timeout error is sent to the
// requests' reply channel.
type topicConsumer struct {
	contextID     *sarama.ContextID
	cfg           *config.T
	gc            *groupConsumer
	group         string
	topic         string
	assignmentsCh chan []int32
	requestsCh    chan consumeRequest
	messagesCh    chan *sarama.ConsumerMessage
	wg            sync.WaitGroup
}

func (gc *groupConsumer) newTopicConsumer(topic string) *topicConsumer {
	return &topicConsumer{
		contextID:     gc.baseCID.NewChild(topic),
		cfg:           gc.cfg,
		gc:            gc,
		group:         gc.group,
		topic:         topic,
		assignmentsCh: make(chan []int32),
		requestsCh:    make(chan consumeRequest, gc.cfg.Consumer.ChannelBufferSize),
		messagesCh:    make(chan *sarama.ConsumerMessage),
	}
}

func (tc *topicConsumer) messages() chan<- *sarama.ConsumerMessage {
	return tc.messagesCh
}

func (tc *topicConsumer) key() string {
	return tc.topic
}

func (tc *topicConsumer) requests() chan<- consumeRequest {
	return tc.requestsCh
}

func (tc *topicConsumer) start(stoppedCh chan<- dispatchTier) {
	spawn(&tc.wg, func() {
		defer func() { stoppedCh <- tc }()
		tc.run()
	})
}

func (tc *topicConsumer) stop() {
	close(tc.requestsCh)
	tc.wg.Wait()
}

func (tc *topicConsumer) run() {
	defer tc.contextID.LogScope()()
	tc.gc.addTopicConsumer() <- tc
	defer func() {
		tc.gc.deleteTopicConsumer() <- tc
	}()

	timeoutErr := ErrRequestTimeout(fmt.Errorf("long polling timeout"))
	timeoutResult := consumeResult{Err: timeoutErr}
	for consumeReq := range tc.requestsCh {
		requestAge := time.Now().UTC().Sub(consumeReq.timestamp)
		ttl := tc.cfg.Consumer.LongPollingTimeout - requestAge
		// The request has been waiting in the buffer for too long. If we
		// reply with a fetched message, then there is a good chance that the
		// client won't receive it due to the client HTTP timeout. Therefore
		// we reject the request to avoid message loss.
		if ttl <= 0 {
			consumeReq.replyCh <- timeoutResult
			continue
		}

		select {
		case msg := <-tc.messagesCh:
			consumeReq.replyCh <- consumeResult{Msg: msg}
		case <-time.After(ttl):
			consumeReq.replyCh <- timeoutResult
		}
	}
}

func (tc *topicConsumer) String() string {
	return tc.contextID.String()
}

// exclusiveConsumer ensures exclusive consumption of messages from a topic
// partition within a particular group. It ensures that a partition is consumed
// exclusively by first claiming the partition in ZooKeeper. When a fetched
// message is pulled from the `messages()` channel, it is considered to be
// consumed and its offset is committed.
type exclusiveConsumer struct {
	contextID    *sarama.ContextID
	cfg          *config.T
	group        string
	topic        string
	partition    int32
	dumbConsumer sarama.Consumer
	registry     *consumerGroupRegistry
	offsetMgr    sarama.OffsetManager
	messagesCh   chan *sarama.ConsumerMessage
	acksCh       chan *sarama.ConsumerMessage
	stoppingCh   chan none
	wg           sync.WaitGroup
}

func (gc *groupConsumer) spawnExclusiveConsumer(topic string, partition int32) *exclusiveConsumer {
	ec := &exclusiveConsumer{
		contextID:    gc.baseCID.NewChild(fmt.Sprintf("%s:%d", topic, partition)),
		cfg:          gc.cfg,
		group:        gc.group,
		topic:        topic,
		partition:    partition,
		dumbConsumer: gc.dumbConsumer,
		registry:     gc.registry,
		offsetMgr:    gc.offsetMgr,
		messagesCh:   make(chan *sarama.ConsumerMessage),
		acksCh:       make(chan *sarama.ConsumerMessage),
		stoppingCh:   make(chan none),
	}
	spawn(&ec.wg, ec.run)
	return ec
}

func (ec *exclusiveConsumer) messages() <-chan *sarama.ConsumerMessage {
	return ec.messagesCh
}

func (ec *exclusiveConsumer) acks() chan<- *sarama.ConsumerMessage {
	return ec.acksCh
}

func (ec *exclusiveConsumer) run() {
	defer ec.contextID.LogScope()()
	defer ec.registry.claimPartition(ec.contextID, ec.topic, ec.partition, ec.stoppingCh)()

	pom, err := ec.offsetMgr.ManagePartition(ec.group, ec.topic, ec.partition)
	if err != nil {
		panic(fmt.Errorf("<%s> failed to spawn partition manager: err=(%s)", ec.contextID, err))
	}
	defer pom.Close()

	// Wait for the initial offset to be retrieved.
	var initialOffset sarama.DecoratedOffset
	select {
	case initialOffset = <-pom.InitialOffset():
	case <-ec.stoppingCh:
		return
	}

	pc, concreteOffset, err := ec.dumbConsumer.ConsumePartition(ec.topic, ec.partition, initialOffset.Offset)
	if err != nil {
		panic(fmt.Errorf("<%s> failed to start partition consumer: err=(%s)", ec.contextID, err))
	}
	defer pc.Close()
	log.Infof("<%s> initialized: initialOffset=%d, concreteOffset=%d",
		ec.contextID, initialOffset.Offset, concreteOffset)

	var lastSubmittedOffset, lastCommittedOffset int64

	// Initialize the Kafka offset storage for a group on first consumption.
	if initialOffset.Offset == sarama.OffsetNewest {
		pom.SubmitOffset(concreteOffset, "")
		lastSubmittedOffset = concreteOffset
	}

	firstMessageFetched := false
	for {
		var msg *sarama.ConsumerMessage
		// Wait for a fetched message to to provided by the controlled
		// partition consumer.
		for {
			select {
			case msg = <-pc.Messages():
				// Notify tests when the very first message is fetched.
				if !firstMessageFetched && firstMessageFetchedCh != nil {
					firstMessageFetched = true
					firstMessageFetchedCh <- ec
				}
				goto offerAndAck
			case committedOffset := <-pom.CommittedOffsets():
				lastCommittedOffset = committedOffset.Offset
				continue
			case <-ec.stoppingCh:
				goto done
			}
		}
	offerAndAck:
		// Offer the fetched message to the upstream consumer and wait for it
		// to be acknowledged.
		for {
			select {
			case ec.messagesCh <- msg:
				// Keep offering the same message until it is acknowledged.
			case <-ec.acksCh:
				lastSubmittedOffset = msg.Offset + 1
				pom.SubmitOffset(lastSubmittedOffset, "")
				break offerAndAck
			case committedOffset := <-pom.CommittedOffsets():
				lastCommittedOffset = committedOffset.Offset
				continue
			case <-ec.stoppingCh:
				goto done
			}
		}
	}
done:
	if lastCommittedOffset == lastSubmittedOffset {
		return
	}
	// It is necessary to wait for the offset of the last consumed message to
	// be committed to Kafka before releasing ownership over the partition,
	// otherwise the message can be consumed by the new partition owner again.
	log.Infof("<%s> waiting for the last offset to be committed: submitted=%d, committed=%d",
		ec.contextID, lastSubmittedOffset, lastCommittedOffset)
	for committedOffset := range pom.CommittedOffsets() {
		if committedOffset.Offset == lastSubmittedOffset {
			return
		}
		log.Infof("<%s> waiting for the last offset to be committed: submitted=%d, committed=%d",
			ec.contextID, lastSubmittedOffset, committedOffset.Offset)
	}
}

func (ec *exclusiveConsumer) stop() {
	close(ec.stoppingCh)
	ec.wg.Wait()
}

type none struct{}

// spawn starts function `f` as a goroutine making it a member of the `wg`
// wait group.
func spawn(wg *sync.WaitGroup, f func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		f()
	}()
}

type Int32Slice []int32

func (p Int32Slice) Len() int           { return len(p) }
func (p Int32Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p Int32Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
