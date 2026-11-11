package dnstap_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// dnstapSchema is dnstap.proto (proto2, CC0) as a descriptor: the messages
// Dnstap and Message with the field numbers, types and labels of the
// published schema. The general protocol buffers runtime decodes the
// hand-written encoder's output against it, required fields included.
func dnstapSchema(t *testing.T) (dnstapMsg, message protoreflect.MessageDescriptor) {
	t.Helper()
	opt, req := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum()
	field := func(name string, num int32, label *descriptorpb.FieldDescriptorProto_Label, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: label, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	enum := func(name string, values ...string) *descriptorpb.EnumDescriptorProto {
		e := &descriptorpb.EnumDescriptorProto{Name: proto.String(name)}
		for i, v := range values {
			if v != "" {
				e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v), Number: proto.Int32(int32(i + 1))})
			}
		}
		return e
	}
	const (
		bytesT   = descriptorpb.FieldDescriptorProto_TYPE_BYTES
		enumT    = descriptorpb.FieldDescriptorProto_TYPE_ENUM
		uint32T  = descriptorpb.FieldDescriptorProto_TYPE_UINT32
		uint64T  = descriptorpb.FieldDescriptorProto_TYPE_UINT64
		fixed32T = descriptorpb.FieldDescriptorProto_TYPE_FIXED32
		msgT     = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	)
	file := &descriptorpb.FileDescriptorProto{
		Name: proto.String("dnstap.proto"), Package: proto.String("dnstap"), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			enum("SocketFamily", "INET", "INET6"),
			enum("SocketProtocol", "UDP", "TCP", "DOT", "DOH", "DNSCryptUDP", "DNSCryptTCP", "DOQ"),
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:     proto.String("Dnstap"),
				EnumType: []*descriptorpb.EnumDescriptorProto{enum("Type", "MESSAGE")},
				Field: []*descriptorpb.FieldDescriptorProto{
					field("identity", 1, opt, bytesT, ""), field("version", 2, opt, bytesT, ""), field("extra", 3, opt, bytesT, ""),
					field("type", 15, req, enumT, ".dnstap.Dnstap.Type"), field("message", 14, opt, msgT, ".dnstap.Message"),
				},
			},
			{
				Name: proto.String("Message"),
				EnumType: []*descriptorpb.EnumDescriptorProto{enum("Type", "AUTH_QUERY", "AUTH_RESPONSE", "RESOLVER_QUERY", "RESOLVER_RESPONSE", "CLIENT_QUERY", "CLIENT_RESPONSE",
					"FORWARDER_QUERY", "FORWARDER_RESPONSE", "STUB_QUERY", "STUB_RESPONSE", "TOOL_QUERY", "TOOL_RESPONSE", "UPDATE_QUERY", "UPDATE_RESPONSE")},
				Field: []*descriptorpb.FieldDescriptorProto{
					field("type", 1, req, enumT, ".dnstap.Message.Type"),
					field("socket_family", 2, opt, enumT, ".dnstap.SocketFamily"), field("socket_protocol", 3, opt, enumT, ".dnstap.SocketProtocol"),
					field("query_address", 4, opt, bytesT, ""), field("response_address", 5, opt, bytesT, ""),
					field("query_port", 6, opt, uint32T, ""), field("response_port", 7, opt, uint32T, ""),
					field("query_time_sec", 8, opt, uint64T, ""), field("query_time_nsec", 9, opt, fixed32T, ""),
					field("query_message", 10, opt, bytesT, ""), field("query_zone", 11, opt, bytesT, ""),
					field("response_time_sec", 12, opt, uint64T, ""), field("response_time_nsec", 13, opt, fixed32T, ""),
					field("response_message", 14, opt, bytesT, ""),
				},
			},
		},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName("Dnstap"), fd.Messages().ByName("Message")
}

// TestFrameMatchesTheDnstapSchema decodes the encoder's output with the
// protocol buffers runtime against dnstap.proto.
func TestFrameMatchesTheDnstapSchema(t *testing.T) {
	top, _ := dnstapSchema(t)
	at := time.Date(2026, 11, 10, 1, 2, 3, 456, time.UTC)
	frame := &dnstap.Frame{Identity: []byte("dns1"), Version: []byte("kudurru/test"), Extra: []byte("status=forwarded"), Message: dnstap.Message{
		Type: dnstap.ClientResponse, SocketFamily: dnstap.FamilyINET6, SocketProtocol: dnstap.ProtocolDOQ,
		QueryAddress: []byte{0x20, 1, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, QueryPort: 853,
		QueryTime: at, QueryMessage: []byte{1, 2, 3}, QueryZone: []byte{4, 'h', 'o', 'm', 'e', 0},
		ResponseTime: at.Add(time.Millisecond), ResponseMessage: []byte{9, 8, 7},
	}}
	encoded, _ := dnstap.AppendFrame(nil, nil, frame)
	decoded := dynamicpb.NewMessage(top)
	if err := proto.Unmarshal(encoded, decoded); err != nil { // checks required fields
		t.Fatal(err)
	}
	if decoded.GetUnknown() != nil {
		t.Fatal("fields unknown to dnstap.proto")
	}
	get := func(m protoreflect.Message, name string) protoreflect.Value {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if !m.Has(fd) {
			t.Fatalf("%s is not set", name)
		}
		return m.Get(fd)
	}
	if string(get(decoded, "identity").Bytes()) != "dns1" || get(decoded, "type").Enum() != 1 {
		t.Fatal("Dnstap fields")
	}
	m := get(decoded, "message").Message()
	if m.GetUnknown() != nil {
		t.Fatal("Message fields unknown to dnstap.proto")
	}
	checks := map[string]any{
		"type": protoreflect.EnumNumber(6), "socket_family": protoreflect.EnumNumber(2), "socket_protocol": protoreflect.EnumNumber(7),
		"query_port": uint32(853), "query_time_sec": uint64(at.Unix()), "query_time_nsec": uint32(456),
		"response_time_sec": uint64(at.Unix()), "response_time_nsec": uint32(1000456),
	}
	for name, want := range checks {
		v := get(m, name)
		var got any
		switch want.(type) {
		case protoreflect.EnumNumber:
			got = v.Enum()
		case uint32:
			got = uint32(v.Uint()) //nolint:gosec // G115: a uint32 field
		default:
			got = v.Uint()
		}
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if string(get(m, "response_message").Bytes()) != "\x09\x08\x07" || len(get(m, "query_address").Bytes()) != 16 {
		t.Fatal("bytes fields")
	}
	// A minimal frame (only the required fields) decodes too, and the zero
	// values are absent rather than sent as zeros.
	minimal, _ := dnstap.AppendFrame(nil, nil, &dnstap.Frame{Message: dnstap.Message{Type: dnstap.AuthQuery}})
	decoded = dynamicpb.NewMessage(top)
	if err := proto.Unmarshal(minimal, decoded); err != nil {
		t.Fatal(err)
	}
	inner := get(decoded, "message").Message()
	if inner.Has(inner.Descriptor().Fields().ByName("query_port")) || inner.Has(inner.Descriptor().Fields().ByName("query_time_sec")) {
		t.Fatal("zero values were sent")
	}
}
