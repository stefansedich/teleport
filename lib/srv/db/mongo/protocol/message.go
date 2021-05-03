/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package protocol

import (
	"io"

	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"

	"github.com/gravitational/trace"
)

const (
	headerSizeBytes = 16
)

// Message defines common interface for MongoDB wire protocol messages.
type Message interface {
	GetHeader() MessageHeader
	GetBytes() []byte
}

// MessageHeader represents parsed MongoDB wire protocol message header.
//
// https://docs.mongodb.com/master/reference/mongodb-wire-protocol/#standard-message-header
type MessageHeader struct {
	MessageLength int32
	RequestID     int32
	ResponseTo    int32
	OpCode        wiremessage.OpCode
	bytes         [headerSizeBytes]byte
}

// MessageOpMsg represents parsed OP_MSG wire message.
//
// https://docs.mongodb.com/master/reference/mongodb-wire-protocol/#op-msg
type MessageOpMsg struct {
	Header   MessageHeader
	Flags    wiremessage.MsgFlag
	Sections []Section
	bytes    []byte
	// There's also optional checksum field which we've no use for.
}

// GetHeader returns the wire message header.
func (m *MessageOpMsg) GetHeader() MessageHeader {
	return m.Header
}

// GetBytes returns the message raw bytes.
func (m *MessageOpMsg) GetBytes() []byte {
	return append(m.Header.bytes[:], m.bytes...)
}

// GetDocuments returns all documents from all sections present in the message.
func (m *MessageOpMsg) GetDocuments() (result []bsoncore.Document) {
	for _, section := range m.Sections {
		switch s := section.(type) {
		case *SectionBody:
			result = append(result, s.Document)
		case *SectionDocumentSequence:
			result = append(result, s.Documents...)
		}
	}
	return result
}

// GetDatabase returns name of the database for the query, or an empty string.
func (m *MessageOpMsg) GetDatabase() string {
	for _, document := range m.GetDocuments() {
		if value := document.Lookup("$db"); value.String() != "" {
			return value.String()
		}
	}
	return ""
}

// Section represents a single OP_MSG wire message section.
//
// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#sections
type Section interface {
	GetType() wiremessage.SectionType
}

// SectionBody represents OP_MSG Body section that contains a single bson
// document.
//
// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#kind-0--body
type SectionBody struct {
	Document bsoncore.Document
}

// GetType returns this section type.
func (s *SectionBody) GetType() wiremessage.SectionType {
	return wiremessage.SingleDocument
}

// SectionDocumentSequence represents OP_MSG Document Sequence section that
// contains multiple bson documents.
//
// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#kind-1--document-sequence
type SectionDocumentSequence struct {
	Identifier string
	Documents  []bsoncore.Document
}

// GetType returns this section type.
func (s *SectionDocumentSequence) GetType() wiremessage.SectionType {
	return wiremessage.DocumentSequence
}

// MessageUnknown represents a wire message we don't currently support.
type MessageUnknown struct {
	Header MessageHeader
	raw    []byte
}

// GetHeader returns the wire message header.
func (m *MessageUnknown) GetHeader() MessageHeader {
	return m.Header
}

// GetBytes returns the message raw bytes.
func (m *MessageUnknown) GetBytes() []byte {
	return append(m.Header.bytes[:], m.raw...)
}

// ReadMessage reads the next MongoDB wire protocol message from the reader.
func ReadMessage(reader io.Reader) (Message, error) {
	header, payload, err := readMessage(reader)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Parse the message body.
	switch header.OpCode {
	case wiremessage.OpMsg:
		return readOpMsg(*header, payload)
	default:
		return &MessageUnknown{
			Header: *header,
			raw:    payload,
		}, nil
	}
}

func readMessage(reader io.Reader) (*MessageHeader, []byte, error) {
	// First read message header which is 16 bytes.
	var header [headerSizeBytes]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, nil, trace.Wrap(err)
	}
	length, requestID, responseTo, opCode, _, ok := wiremessage.ReadHeader(header[:])
	if !ok {
		return nil, nil, trace.BadParameter("failed to read message header %v", header)
	}
	// Then read the entire message body.
	payload := make([]byte, length-headerSizeBytes)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return &MessageHeader{
		MessageLength: length,
		RequestID:     requestID,
		ResponseTo:    responseTo,
		OpCode:        opCode,
		bytes:         header,
	}, payload, nil
}

func readOpMsg(header MessageHeader, payload []byte) (*MessageOpMsg, error) {
	flags, rem, ok := wiremessage.ReadMsgFlags(payload)
	if !ok {
		return nil, trace.BadParameter("failed to read OP_MSG flags %v", payload)
	}
	var sections []Section
	for len(rem) > 0 {
		var sectionType wiremessage.SectionType
		sectionType, rem, ok = wiremessage.ReadMsgSectionType(rem)
		if !ok {
			return nil, trace.BadParameter("failed to read OP_MSG section type %v", payload)
		}
		switch sectionType {
		case wiremessage.SingleDocument:
			var doc bsoncore.Document
			doc, rem, ok = wiremessage.ReadMsgSectionSingleDocument(rem)
			if !ok {
				return nil, trace.BadParameter("failed to read OP_MSG body section %v", payload)
			}
			sections = append(sections, &SectionBody{
				Document: doc,
			})
		case wiremessage.DocumentSequence:
			var id string
			var docs []bsoncore.Document
			id, docs, rem, ok = wiremessage.ReadMsgSectionDocumentSequence(rem)
			if !ok {
				return nil, trace.BadParameter("failed to read OP_MSG document sequence section %v", payload)
			}
			sections = append(sections, &SectionDocumentSequence{
				Identifier: id,
				Documents:  docs,
			})
		}
	}
	return &MessageOpMsg{
		Header:   header,
		Flags:    flags,
		Sections: sections,
		bytes:    payload,
	}, nil
}
