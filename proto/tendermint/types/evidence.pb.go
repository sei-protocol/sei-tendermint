// Code generated by protoc-gen-gogo. DO NOT EDIT.
// source: tendermint/types/evidence.proto

package types

import (
	fmt "fmt"
	_ "github.com/gogo/protobuf/gogoproto"
	proto "github.com/gogo/protobuf/proto"
	_ "github.com/gogo/protobuf/types"
	math "math"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.GoGoProtoPackageIsVersion3 // please upgrade the proto package

func init() { proto.RegisterFile("tendermint/types/evidence.proto", fileDescriptor_6825fabc78e0a168) }

var fileDescriptor_6825fabc78e0a168 = []byte{
	// 177 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xe2, 0x92, 0x2f, 0x49, 0xcd, 0x4b,
	0x49, 0x2d, 0xca, 0xcd, 0xcc, 0x2b, 0xd1, 0x2f, 0xa9, 0x2c, 0x48, 0x2d, 0xd6, 0x4f, 0x2d, 0xcb,
	0x4c, 0x49, 0xcd, 0x4b, 0x4e, 0xd5, 0x2b, 0x28, 0xca, 0x2f, 0xc9, 0x17, 0x12, 0x40, 0x28, 0xd0,
	0x03, 0x2b, 0x90, 0x12, 0x49, 0xcf, 0x4f, 0xcf, 0x07, 0x4b, 0xea, 0x83, 0x58, 0x10, 0x75, 0x52,
	0xf2, 0xe9, 0xf9, 0xf9, 0xe9, 0x39, 0xa9, 0xfa, 0x60, 0x5e, 0x52, 0x69, 0x9a, 0x7e, 0x49, 0x66,
	0x6e, 0x6a, 0x71, 0x49, 0x62, 0x6e, 0x01, 0x54, 0x81, 0x0c, 0x86, 0x4d, 0x60, 0x12, 0x2a, 0xab,
	0x80, 0x21, 0x5b, 0x96, 0x98, 0x93, 0x99, 0x92, 0x58, 0x92, 0x5f, 0x04, 0x51, 0xe1, 0x14, 0x78,
	0xe2, 0x91, 0x1c, 0xe3, 0x85, 0x47, 0x72, 0x8c, 0x0f, 0x1e, 0xc9, 0x31, 0x4e, 0x78, 0x2c, 0xc7,
	0x70, 0xe1, 0xb1, 0x1c, 0xc3, 0x8d, 0xc7, 0x72, 0x0c, 0x51, 0xe6, 0xe9, 0x99, 0x25, 0x19, 0xa5,
	0x49, 0x7a, 0xc9, 0xf9, 0xb9, 0xfa, 0xc8, 0xc6, 0x20, 0x98, 0x10, 0xd7, 0xa2, 0x5b, 0x91, 0xc4,
	0x06, 0x16, 0x37, 0x06, 0x04, 0x00, 0x00, 0xff, 0xff, 0x8e, 0x24, 0xe7, 0x8c, 0x05, 0x01, 0x00,
	0x00,
}
