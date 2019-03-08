package kafka

import (
	"bytes"
	"encoding/binary"
	"fmt"
	log "github.com/sirupsen/logrus"
)

type offsetGroupMetadata struct {
	Header  offsetGroupMetadataHeader
	Members []offsetGroupMetadataMember
}

type offsetGroupMetadataHeader struct {
	ProtocolType string
	Generation   int32
	Protocol     string
	Leader       string
}

type offsetGroupMetadataMember struct {
	MemberID         string
	ClientID         string
	ClientHost       string
	RebalanceTimeout int32
	SessionTimeout   int32
	Assignment       map[string][]int32
}

func newOffsetGroupMetadata(keyBuffer *bytes.Buffer, value []byte, logger *log.Entry) (*offsetGroupMetadata, error) {
	// Decode key (resolves to group id)
	group, err := readString(keyBuffer)
	if err != nil {
		logger.WithFields(log.Fields{
			"message_type": "metadata",
			"reason":       "group",
		}).Warn("failed to decode")
		return nil, err
	}

	// Decode value version
	var valueVersion int16
	valueBuffer := bytes.NewBuffer(value)
	err = binary.Read(valueBuffer, binary.BigEndian, &valueVersion)
	if err != nil {
		logger.WithFields(log.Fields{
			"message_type": "metadata",
			"reason":       "no value version",
			"group":        group,
		}).Warn("failed to decode")

		return nil, err
	}

	// Decode value content
	switch valueVersion {
	case 0, 1:
		metadata, err := decodeGroupMetadata(valueVersion, group, valueBuffer, logger.WithFields(log.Fields{
			"message_type": "metadata",
			"group":        group,
		}))

		return metadata, err
	default:
		logger.WithFields(log.Fields{
			"message_type": "metadata",
			"group":        group,
			"reason":       "value version",
			"version":      valueVersion,
		}).Warn("failed to decode")

		return nil, fmt.Errorf("Failed to decode group metadata because value version is not supported")
	}
}

func decodeGroupMetadata(valueVersion int16, group string, valueBuffer *bytes.Buffer, logger *log.Entry) (*offsetGroupMetadata, error) {
	// First decode header fields
	var err error
	metadataHeader := offsetGroupMetadataHeader{}
	metadataHeader.ProtocolType, err = readString(valueBuffer)
	if err != nil {
		logger.WithFields(log.Fields{
			"reason": "metadata header protocol type",
		}).Warn("failed to decode")
		return nil, err
	}
	err = binary.Read(valueBuffer, binary.BigEndian, &metadataHeader.Generation)
	if err != nil {
		logger.WithFields(log.Fields{
			"reason":        "metadata header generation",
			"protocol_type": metadataHeader.ProtocolType,
		}).Warn("failed to decode")
		return nil, err
	}
	metadataHeader.Protocol, err = readString(valueBuffer)
	if err != nil {
		logger.WithFields(log.Fields{
			"reason":        "metadata header protocol",
			"protocol_type": metadataHeader.ProtocolType,
			"generation":    metadataHeader.Generation,
		}).Warn("failed to decode")
		return nil, err
	}
	metadataHeader.Leader, err = readString(valueBuffer)
	if err != nil {
		logger.WithFields(log.Fields{
			"reason":        "metadata header leader",
			"protocol_type": metadataHeader.ProtocolType,
			"generation":    metadataHeader.Generation,
			"protocol":      metadataHeader.Protocol,
		}).Warn("failed to decode")
		return nil, err
	}

	// Now decode metadata members
	metadataLogger := logger.WithFields(log.Fields{
		"protocol_type": metadataHeader.ProtocolType,
		"generation":    metadataHeader.Generation,
		"protocol":      metadataHeader.Protocol,
		"leader":        metadataHeader.Leader,
	})

	var memberCount int32
	err = binary.Read(valueBuffer, binary.BigEndian, &memberCount)
	if err != nil {
		metadataLogger.WithFields(log.Fields{
			"reason": "no member size",
		}).Warn("failed to decode")
		return nil, err
	}

	for i := 0; i < int(memberCount); i++ {
		member, errorAt := decodeMetadataMember(valueBuffer, valueVersion)
		if errorAt != "" {
			metadataLogger.WithFields(log.Fields{
				"reason": errorAt,
			}).Warn("Failed to decode")

			return nil, fmt.Errorf("Decoding member, error at: %v", errorAt)
		}

		for topic, partitions := range member.Assignment {
			for _, partition := range partitions {
				metadataLogger.WithFields(log.Fields{
					"topic":     topic,
					"partition": partition,
					"group":     group,
					"owner":     member.ClientHost,
					"client_id": member.ClientID,
				}).Info("Got group metadata")
			}
		}
	}

	return &offsetGroupMetadata{}, nil
}

func decodeMetadataMember(buf *bytes.Buffer, memberVersion int16) (offsetGroupMetadataMember, string) {
	var err error
	memberMetadata := offsetGroupMetadataMember{}

	memberMetadata.MemberID, err = readString(buf)
	if err != nil {
		return memberMetadata, "member_id"
	}
	memberMetadata.ClientID, err = readString(buf)
	if err != nil {
		return memberMetadata, "client_id"
	}
	memberMetadata.ClientHost, err = readString(buf)
	if err != nil {
		return memberMetadata, "client_host"
	}
	if memberVersion == 1 {
		err = binary.Read(buf, binary.BigEndian, &memberMetadata.RebalanceTimeout)
		if err != nil {
			return memberMetadata, "rebalance_timeout"
		}
	}
	err = binary.Read(buf, binary.BigEndian, &memberMetadata.SessionTimeout)
	if err != nil {
		return memberMetadata, "session_timeout"
	}

	var subscriptionBytes int32
	err = binary.Read(buf, binary.BigEndian, &subscriptionBytes)
	if err != nil {
		return memberMetadata, "subscription_bytes"
	}
	if subscriptionBytes > 0 {
		buf.Next(int(subscriptionBytes))
	}

	var assignmentBytes int32
	err = binary.Read(buf, binary.BigEndian, &assignmentBytes)
	if err != nil {
		return memberMetadata, "assignment_bytes"
	}

	if assignmentBytes > 0 {
		assignmentData := buf.Next(int(assignmentBytes))
		assignmentBuf := bytes.NewBuffer(assignmentData)
		var consumerProtocolVersion int16
		err = binary.Read(assignmentBuf, binary.BigEndian, &consumerProtocolVersion)
		if err != nil {
			return memberMetadata, "consumer_protocol_version"
		}
		if consumerProtocolVersion < 0 {
			return memberMetadata, "consumer_protocol_version"
		}
		assignment, errorAt := decodeMemberAssignmentV0(assignmentBuf)
		if errorAt != "" {
			return memberMetadata, "assignment"
		}
		memberMetadata.Assignment = assignment
	}

	return memberMetadata, ""
}

func decodeMemberAssignmentV0(buf *bytes.Buffer) (map[string][]int32, string) {
	var err error
	var topics map[string][]int32
	var numTopics, numPartitions, partitionID, userDataLen int32

	err = binary.Read(buf, binary.BigEndian, &numTopics)
	if err != nil {
		return topics, "assignment_topic_count"
	}

	topicCount := int(numTopics)
	topics = make(map[string][]int32, numTopics)
	for i := 0; i < topicCount; i++ {
		topicName, err := readString(buf)
		if err != nil {
			return topics, "topic_name"
		}

		err = binary.Read(buf, binary.BigEndian, &numPartitions)
		if err != nil {
			return topics, "assignment_partition_count"
		}
		partitionCount := int(numPartitions)
		topics[topicName] = make([]int32, numPartitions)
		for j := 0; j < partitionCount; j++ {
			err = binary.Read(buf, binary.BigEndian, &partitionID)
			if err != nil {
				return topics, "assignment_partition_id"
			}
			topics[topicName][j] = int32(partitionID)
		}
	}

	err = binary.Read(buf, binary.BigEndian, &userDataLen)
	if err != nil {
		return topics, "user_bytes"
	}
	if userDataLen > 0 {
		buf.Next(int(userDataLen))
	}

	return topics, ""
}
