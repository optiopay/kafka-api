package kafka

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"time"
)

// FetchReq encapsulates a request to fetch data.
type FetchReq struct {
	CorrelationID int32
	ClientID      string
	MaxWaitTime   time.Duration
	MinBytes      int32

	Sources []FetchReqTopic
}

// FetchReqTopic points to partition offsets in a Kafka topic.
type FetchReqTopic struct {
	Topic      string
	Partitions []FetchReqPartition
}

// FetchReqPartition tells the fetcher where to fetch from.
type FetchReqPartition struct {
	Partition   int32
	FetchOffset int64
	MaxBytes    int32
}

// FetchResp encapsulates the response from a FetchReq.
type FetchResp struct {
	CorrelationID int32
	Sources       []FetchRespTopic
}

// FetchRespTopic points to a Kafka topic and partition offsets.
type FetchRespTopic struct {
	Topic      string
	Partitions []FetchRespPartition
}

// FetchRespPartition points to a specific offset and list of Messages.
type FetchRespPartition struct {
	Partition int32
	Err       error
	TipOffset int64
	Messages  []*Message
}

// Message encapsualtes a Kafka message.
type Message struct {
	Offset int64
	Crc    uint32
	Key    []byte
	Value  []byte
}

// Bytes encodes a Message to a list of bytes.
func (m *Message) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := newEncoder(&buf)

	enc.Encode(int32(0)) // crc placeholder
	enc.Encode(int8(0))  // magic byte is always 0
	enc.Encode(int8(0))  // no compress support
	enc.Encode(m.Key)
	enc.Encode(m.Value)

	if enc.Err() != nil {
		return nil, enc.Err()
	}

	b := buf.Bytes()
	binary.BigEndian.PutUint32(b, crc32.ChecksumIEEE(b[4:])) // update crc
	return b, nil
}

// Bytes coverts a FetchReq object to a list of bytes.
func (r *FetchReq) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := newEncoder(&buf)

	enc.Encode(int32(0)) // message size
	enc.Encode(int16(reqFetch))
	enc.Encode(int16(0))
	enc.Encode(r.CorrelationID)
	enc.Encode(r.ClientID)
	enc.Encode(int32(-1)) // replica id
	enc.Encode(int32(r.MaxWaitTime / time.Millisecond))
	enc.Encode(r.MinBytes)

	enc.EncodeArrayLen(len(r.Sources))
	for _, source := range r.Sources {
		enc.Encode(source.Topic)
		enc.EncodeArrayLen(len(source.Partitions))
		for _, part := range source.Partitions {
			enc.Encode(part.Partition)
			enc.Encode(part.FetchOffset)
			enc.Encode(part.MaxBytes)
		}
	}

	if enc.Err() != nil {
		return nil, enc.Err()
	}

	b := buf.Bytes()
	binary.BigEndian.PutUint32(b, uint32(len(b)-4)) // update message size
	return b, nil
}

// WriteTo converts a FetchReq to bytes and returns a count of bytes written.
func (r *FetchReq) WriteTo(w io.Writer) (int64, error) {
	b, err := r.Bytes()
	if err != nil {
		return 0, err
	}
	n, err := w.Write(b)
	return int64(n), err
}

// ReadFetchResp populates a FetchResp object with Kafka Messages.
func ReadFetchResp(r io.Reader) (*FetchResp, error) {
	var err error
	var resp FetchResp
	dec := newDecoder(r)

	_ = dec.DecodeInt32() // message size
	resp.CorrelationID = dec.DecodeInt32()

	resp.Sources = make([]FetchRespTopic, dec.DecodeArrayLen())
	for sourceIndex := range resp.Sources {
		var source = &resp.Sources[sourceIndex]
		source.Topic = dec.DecodeString()
		source.Partitions = make([]FetchRespPartition, dec.DecodeArrayLen())
		for partIndex := range source.Partitions {
			var part = &source.Partitions[partIndex]
			part.Partition = dec.DecodeInt32()
			part.Err = errFromNo(dec.DecodeInt16())
			part.TipOffset = dec.DecodeInt64()
			messagesSetSize := dec.DecodeInt32()

			if dec.Err() != nil {
				return nil, dec.Err()
			}
			if part.Messages, err = readMessageSet(io.LimitReader(r, int64(messagesSetSize))); err != nil {
				return nil, err
			}
		}
	}

	if dec.Err() != nil {
		return nil, dec.Err()
	}

	return &resp, nil
}

// readMessageSet read in each Kafka message until EOF.
func readMessageSet(r io.Reader) ([]*Message, error) {
	set := make([]*Message, 0, 32)
	dec := newDecoder(r)
	msg := &Message{}

	var offset int64
	var attributes int8
	var err error

	for {
		offset = dec.DecodeInt64()
		if err = dec.Err(); err != nil {
			if err == io.EOF {
				return set, nil
			}
			return nil, err
		}

		_ = dec.DecodeInt32() // single message size
		msg.Offset = offset
		msg.Crc = dec.DecodeUint32() // TODO(husio) check crc
		_ = dec.DecodeInt8()         // magic byte

		attributes = dec.DecodeInt8()
		if attributes != compressNone {
			return nil, errors.New("cannot read compressed message") // TODO(husio)
		}

		msg.Key = dec.DecodeBytes()
		msg.Value = dec.DecodeBytes()
		set = append(set, msg)
	}
}
