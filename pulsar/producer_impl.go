// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pulsar

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/apache/pulsar-client-go/pulsar/internal"
)

type producer struct {
	sync.Mutex
	client        *client
	options       *ProducerOptions
	topic         string
	producers     []Producer
	numPartitions uint32
	messageRouter func(*ProducerMessage, TopicMetadata) int
	ticker        *time.Ticker

	log *log.Entry
}

const defaultBatchingMaxPublishDelay = 10 * time.Millisecond

var partitionsAutoDiscoveryInterval = 1 * time.Minute

func getHashingFunction(s HashingScheme) func(string) uint32 {
	switch s {
	case JavaStringHash:
		return internal.JavaStringHash
	case Murmur3_32Hash:
		return internal.Murmur3_32Hash
	default:
		return internal.JavaStringHash
	}
}

func newProducer(client *client, options *ProducerOptions) (*producer, error) {
	if options.Topic == "" {
		return nil, newError(ResultInvalidTopicName, "Topic name is required for producer")
	}

	p := &producer{
		options: options,
		topic:   options.Topic,
		client:  client,
		log:     log.WithField("topic", options.Topic),
	}

	var batchingMaxPublishDelay time.Duration
	if options.BatchingMaxPublishDelay != 0 {
		batchingMaxPublishDelay = options.BatchingMaxPublishDelay
	} else {
		batchingMaxPublishDelay = defaultBatchingMaxPublishDelay
	}

	if options.MessageRouter == nil {
		internalRouter := internal.NewDefaultRouter(
			internal.NewSystemClock(),
			getHashingFunction(options.HashingScheme),
			batchingMaxPublishDelay, options.DisableBatching)
		p.messageRouter = func(message *ProducerMessage, metadata TopicMetadata) int {
			return internalRouter(message.Key, metadata.NumPartitions())
		}
	} else {
		p.messageRouter = options.MessageRouter
	}

	err := p.internalCreatePartitionsProducers()
	if err != nil {
		return nil, err
	}

	p.ticker = time.NewTicker(partitionsAutoDiscoveryInterval)

	go func() {
		for range p.ticker.C {
			p.log.Debug("Auto discovering new partitions")
			p.internalCreatePartitionsProducers()
		}
	}()

	return p, nil
}

func (p *producer) internalCreatePartitionsProducers() error {
	partitions, err := p.client.TopicPartitions(p.topic)
	if err != nil {
		return err
	}

	oldNumPartitions := 0
	newNumPartitions := len(partitions)

	p.Lock()
	defer p.Unlock()
	oldProducers := p.producers

	if oldProducers != nil {
		oldNumPartitions = len(oldProducers)
		if oldNumPartitions == newNumPartitions {
			p.log.Debug("Number of partitions in topic has not changed")
			return nil
		}

		p.log.WithField("old_partitions", oldNumPartitions).
			WithField("new_partitions", newNumPartitions).
			Info("Changed number of partitions in topic")
	}

	p.producers = make([]Producer, newNumPartitions)

	// Copy over the existing consumer instances
	for i := 0; i < oldNumPartitions; i++ {
		p.producers[i] = oldProducers[i]
	}

	type ProducerError struct {
		partition int
		prod      Producer
		err       error
	}

	partitionsToAdd := newNumPartitions - oldNumPartitions
	c := make(chan ProducerError, partitionsToAdd)

	for partitionIdx := oldNumPartitions; partitionIdx < newNumPartitions; partitionIdx++ {
		partition := partitions[partitionIdx]

		go func(partitionIdx int, partition string) {
			prod, e := newPartitionProducer(p.client, partition, p.options, partitionIdx)
			c <- ProducerError{
				partition: partitionIdx,
				prod:      prod,
				err:       e,
			}
		}(partitionIdx, partition)
	}

	for i := 0; i < partitionsToAdd; i++ {
		pe, ok := <-c
		if ok {
			if pe.err != nil {
				err = pe.err
			} else {
				p.producers[pe.partition] = pe.prod
			}
		}
	}

	if err != nil {
		// Since there were some failures, cleanup all the partitions that succeeded in creating the producers
		for _, producer := range p.producers {
			if producer != nil {
				producer.Close()
			}
		}
		return err
	}

	atomic.StoreUint32(&p.numPartitions, uint32(len(p.producers)))
	return nil
}

func (p *producer) Topic() string {
	return p.topic
}

func (p *producer) Name() string {
	p.Lock()
	defer p.Unlock()

	return p.producers[0].Name()
}

func (p *producer) NumPartitions() uint32 {
	return atomic.LoadUint32(&p.numPartitions)
}

func (p *producer) Send(ctx context.Context, msg *ProducerMessage) (MessageID, error) {
	p.Lock()
	partition := p.messageRouter(msg, p)
	pp := p.producers[partition]
	p.Unlock()

	return pp.Send(ctx, msg)
}

func (p *producer) SendAsync(ctx context.Context, msg *ProducerMessage,
	callback func(MessageID, *ProducerMessage, error)) {
	p.Lock()
	partition := p.messageRouter(msg, p)
	pp := p.producers[partition]
	p.Unlock()

	pp.SendAsync(ctx, msg, callback)
}

func (p *producer) LastSequenceID() int64 {
	p.Lock()
	defer p.Unlock()

	var maxSeq int64 = -1
	for _, pp := range p.producers {
		s := pp.LastSequenceID()
		if s > maxSeq {
			maxSeq = s
		}
	}
	return maxSeq
}

func (p *producer) Flush() error {
	p.Lock()
	defer p.Unlock()

	for _, pp := range p.producers {
		if err := pp.Flush(); err != nil {
			return err
		}

	}
	return nil
}

func (p *producer) Close() {
	p.Lock()
	defer p.Unlock()

	for _, pp := range p.producers {
		pp.Close()
	}
	p.client.handlers.Del(p)
}