package network

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/perlin-network/noise/crypto"
	"github.com/perlin-network/noise/protobuf"
	"github.com/xtaci/smux"
	"io"
	"time"
)

// sendMessage marshals, signs and sends a message over a stream.
func (n *Network) sendMessage(session *smux.Session, message *protobuf.Message) error {
	stream, err := session.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()

	err = stream.SetDeadline(time.Now().Add(1 * time.Second))
	if err != nil {
		return err
	}

	defer func() {
		stream.SetDeadline(time.Time{})
	}()

	bytes, err := proto.Marshal(message)
	if err != nil {
		return err
	}

	// Serialize size.
	buffer := make([]byte, binary.MaxVarintLen64)
	binary.PutUvarint(buffer, uint64(len(bytes)))

	// Prefix message with its size.
	bytes = append(buffer, bytes...)

	writer := bufio.NewWriter(stream)

	// Send request bytes.
	written, err := writer.Write(bytes)
	if err != nil {
		return err
	}

	// Flush writer.
	err = writer.Flush()
	if err != nil {
		return err
	}

	if written != len(bytes) {
		return fmt.Errorf("only wrote %d / %d bytes to stream", written, len(bytes))
	}

	return nil
}

// receiveMessage reads, unmarshals and verifies a message from a stream.
func (n *Network) receiveMessage(session *smux.Session, timeout time.Time) (*protobuf.Message, error) {
	stream, err := session.AcceptStream()
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	err = stream.SetDeadline(timeout)
	if err != nil {
		return nil, err
	}

	defer func() {
		stream.SetDeadline(time.Time{})
	}()

	reader := bufio.NewReader(stream)

	buffer := make([]byte, binary.MaxVarintLen64)

	_, err = reader.Read(buffer)
	if err != nil {
		return nil, err
	}

	// Decode unsigned varint representing message size.
	size, read := binary.Uvarint(buffer)

	// Check if unsigned varint overflows, or if protobuf message is too large.
	// Message size at most is limited to 4MB. If a big message need be sent,
	// consider partitioning to message into chunks of 4MB.
	if read <= 0 || size > 4e+6 {
		return nil, errors.New("message len is either broken or too large")
	}

	// Read message from buffered I/O completely.
	buffer = make([]byte, size)
	_, err = io.ReadFull(reader, buffer)

	if err != nil {
		// Potentially malicious or dead client; kill it.
		if err == io.ErrUnexpectedEOF {
			stream.Close()
		}
		return nil, err
	}

	// Deserialize message.
	msg := new(protobuf.Message)

	err = proto.Unmarshal(buffer, msg)
	if err != nil {
		return nil, err
	}

	// Check if any of the message headers are invalid or null.
	if msg.Message == nil || msg.Sender == nil || msg.Sender.PublicKey == nil || len(msg.Sender.Address) == 0 || msg.Signature == nil {
		return nil, errors.New("received an invalid message (either no message, no sender, or no signature) from a peer")
	}

	// Verify signature of message.
	if !crypto.Verify(msg.Sender.PublicKey, msg.Message.Value, msg.Signature) {
		return nil, errors.New("received message had an malformed signature")
	}

	return msg, nil
}

// Write asynchronously emit a message to a denoted target address.
func (n *Network) Write(address string, message *protobuf.Message) error {
	packet := &Packet{RemoteAddress: address, Payload: message, Result: make(chan interface{}, 1)}

	n.SendQueue <- packet

	select {
	case raw := <-packet.Result:
		switch result := raw.(type) {
		case error:
			return result
		default:
			return nil
		}
	case <-time.After(3 * time.Second):
	}

	return errors.New("timed out writing message")
}
