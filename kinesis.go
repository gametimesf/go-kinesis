// Package kinesis implements a batch producer built on top of the official AWS SDK.
package kinesis

import (
	"errors"
	"time"

	"github.com/apex/log"
	k "github.com/aws/aws-sdk-go/service/kinesis"
)

const (
	megaByte       = 1 << 20
	maxRecordSize  = megaByte
	maxRequestSize = 5 * megaByte
)

// Errors.
var (
	ErrRecordSizeExceeded = errors.New("kinesis: record size exceeded")
)

// Producer batches records.
type Producer struct {
	Config
	records chan *k.PutRecordsRequestEntry
	done    chan struct{}
}

// New producer with the given config.
func New(config Config) *Producer {
	config.defaults()
	return &Producer{
		Config:  config,
		records: make(chan *k.PutRecordsRequestEntry, config.BacklogSize),
		done:    make(chan struct{}),
	}
}

// Put record `data` using `partitionKey`. This method is thread-safe.
func (p *Producer) Put(data []byte, partitionKey string) error {
	if len(data)+len(partitionKey)+len(p.Config.Separator) > maxRecordSize {
		return ErrRecordSizeExceeded
	}

	p.records <- &k.PutRecordsRequestEntry{
		Data:         append(data, p.Config.Separator...),
		PartitionKey: &partitionKey,
	}

	return nil
}

// Start the producer.
func (p *Producer) Start() {
	go p.loop()
}

// Stop the producer. Flushes any in-flight data.
func (p *Producer) Stop() {
	p.Logger.WithField("backlog", len(p.records)).Info("stopping producer")

	// drain
	p.done <- struct{}{}
	close(p.records)

	// wait
	<-p.done

	p.Logger.Info("stopped producer")
}

// loop and flush at the configured interval, or when the buffer is exceeded.
func (p *Producer) loop() {
	buf := make([]*k.PutRecordsRequestEntry, 0, p.BufferSize)
	bufSize := 0
	tick := time.NewTicker(p.FlushInterval)
	drain := false

	defer tick.Stop()
	defer close(p.done)

	for {
		select {
		case record := <-p.records:
			recordSize := len(*record.PartitionKey) + len(record.Data)

			if bufSize+recordSize > maxRequestSize {
				p.flush(buf, "request size")
				buf = nil
				bufSize = 0
			}

			buf = append(buf, record)
			bufSize += recordSize

			if len(buf) >= p.BufferSize {
				p.flush(buf, "buffer size")
				buf = nil
				bufSize = 0
			}

			if drain && len(p.records) == 0 {
				p.Logger.Info("drained")
				return
			}
		case <-tick.C:
			if len(buf) > 0 {
				p.flush(buf, "interval")
				buf = nil
				bufSize = 0
			}
		case <-p.done:
			drain = true

			if len(p.records) == 0 {
				return
			}
		}
	}
}

// flush records and retry failures if necessary.
func (p *Producer) flush(records []*k.PutRecordsRequestEntry, reason string) {

	out, err := p.Client.PutRecords(&k.PutRecordsInput{
		StreamName: &p.StreamName,
		Records:    records,
	})

	if err != nil {
		p.Logger.WithError(err).Error("flush")
		p.backoff(len(records))
		p.flush(records, "error")
		return
	}

	failed := *out.FailedRecordCount

	if failed == 0 {
		p.Backoff.Reset()
		return
	}

	for _, r := range out.Records {
		if r.ErrorCode == nil {
			continue
		}

		p.Logger.WithFields(log.Fields{
			"code":    *r.ErrorCode,
			"message": *r.ErrorMessage,
		}).Error("push record")
	}

	p.backoff(int(failed))
	p.flush(failures(records, out.Records), "retry")
}

// calculates backoff duration and pauses execution
func (p *Producer) backoff(failed int) {
	backoff := p.Backoff.Duration()

	p.Logger.WithFields(log.Fields{
		"failures": failed,
		"backoff":  backoff,
	}).Warn("put failures")

	time.Sleep(backoff)
}

// failures returns the failed records as indicated in the response.
func failures(records []*k.PutRecordsRequestEntry, response []*k.PutRecordsResultEntry) (out []*k.PutRecordsRequestEntry) {
	for i, record := range response {
		if record.ErrorCode != nil {
			out = append(out, records[i])
		}
	}
	return
}
