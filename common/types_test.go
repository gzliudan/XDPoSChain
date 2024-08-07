// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package common

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

func TestBytesConversion(t *testing.T) {
	bytes := []byte{5}
	hash := BytesToHash(bytes)

	var exp Hash
	exp[31] = 5

	if hash != exp {
		t.Errorf("expected %x got %x", exp, hash)
	}
}

func TestIsHexAddress(t *testing.T) {
	tests := []struct {
		str string
		exp bool
	}{
		{"xdc5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", true},
		{"5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", true},
		{"XDC5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", true},
		{"XdcAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"xdcAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"xdc5aaeb6053f3e94c9b9a09f33669435e7ef1beaed1", false},
		{"xdc5aaeb6053f3e94c9b9a09f33669435e7ef1beae", false},
		{"5aaeb6053f3e94c9b9a09f33669435e7ef1beaed11", false},
		{"xdcxaaeb6053f3e94c9b9a09f33669435e7ef1beaed", false},
	}

	for _, test := range tests {
		if result := IsHexAddress(test.str); result != test.exp {
			t.Errorf("IsHexAddress(%s) == %v; expected %v",
				test.str, result, test.exp)
		}
	}
}

func TestHashJsonValidation(t *testing.T) {
	var tests = []struct {
		Prefix string
		Size   int
		Error  string
	}{
		{"", 62, "json: cannot unmarshal hex string without 0x prefix into Go value of type common.Hash"},
		{"0x", 66, "hex string has length 66, want 64 for common.Hash"},
		{"0x", 63, "json: cannot unmarshal hex string of odd length into Go value of type common.Hash"},
		{"0x", 0, "hex string has length 0, want 64 for common.Hash"},
		{"0x", 64, ""},
		{"0X", 64, ""},
	}
	for _, test := range tests {
		input := `"` + test.Prefix + strings.Repeat("0", test.Size) + `"`
		var v Hash
		err := json.Unmarshal([]byte(input), &v)
		if err == nil {
			if test.Error != "" {
				t.Errorf("%s: error mismatch: have nil, want %q", input, test.Error)
			}
		} else {
			if err.Error() != test.Error {
				t.Errorf("%s: error mismatch: have %q, want %q", input, err, test.Error)
			}
		}
	}
}

func TestAddressUnmarshalJSON(t *testing.T) {
	var tests = []struct {
		Input     string
		ShouldErr bool
		Output    *big.Int
	}{
		{"", true, nil},
		{`""`, true, nil},
		{`"0x"`, true, nil},
		{`"0x00"`, true, nil},
		{`"0xG000000000000000000000000000000000000000"`, true, nil},
		{`"0x0000000000000000000000000000000000000000"`, false, big.NewInt(0)},
		{`"0x0000000000000000000000000000000000000010"`, false, big.NewInt(16)},
		{`"xdc"`, true, nil},
		{`"xdc00"`, true, nil},
		{`"xdcG000000000000000000000000000000000000000"`, true, nil},
		{`"xdc0000000000000000000000000000000000000000"`, false, big.NewInt(0)},
		{`"xdc0000000000000000000000000000000000000010"`, false, big.NewInt(16)},
	}
	for i, test := range tests {
		var v Address
		err := json.Unmarshal([]byte(test.Input), &v)
		if err != nil && !test.ShouldErr {
			t.Errorf("test #%d: unexpected error: %v", i, err)
		}
		if err == nil {
			if test.ShouldErr {
				t.Errorf("test #%d: expected error, got none", i)
			}
			if v.Big().Cmp(test.Output) != 0 {
				t.Errorf("test #%d: address mismatch: have %v, want %v", i, v.Big(), test.Output)
			}
		}
	}
}

func TestAddressHexChecksum(t *testing.T) {
	var tests = []struct {
		Input  string
		Output string
	}{
		// Test cases from https://github.com/ethereum/EIPs/blob/master/EIPS/eip-55.md#specification
		{"xdc5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", "xdc5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"},
		{"xdcfb6916095ca1df60bb79ce92ce3ea74c37c5d359", "xdcfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"},
		{"xdcdbf03b407c01e7cd3cbea99509d93f8dddc8c6fb", "xdcdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB"},
		{"xdcd1220a0cf47c7b9be7a2e6ba89f429762e7b9adb", "xdcD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb"},
		// Ensure that non-standard length input values are handled correctly
		{"0xa", "xdc000000000000000000000000000000000000000A"},
		{"0x0a", "xdc000000000000000000000000000000000000000A"},
		{"0x00a", "xdc000000000000000000000000000000000000000A"},
		{"0x000000000000000000000000000000000000000a", "xdc000000000000000000000000000000000000000A"},
	}
	for i, test := range tests {
		output := HexToAddress(test.Input).Hex()
		if output != test.Output {
			t.Errorf("test #%d: failed to match when it should (%s != %s)", i, output, test.Output)
		}
	}
}

func BenchmarkAddressHex(b *testing.B) {
	testAddr := HexToAddress("0xdc5aaeb6053f3e94c9b9a09f33669435e7ef1beaed")
	for n := 0; n < b.N; n++ {
		testAddr.Hex()
	}
}

func TestRemoveItemInArray(t *testing.T) {
	array := []Address{HexToAddress("0x0000003"), HexToAddress("0x0000001"), HexToAddress("0x0000002"), HexToAddress("0x0000003")}
	remove := []Address{HexToAddress("0x0000002"), HexToAddress("0x0000004"), HexToAddress("0x0000003")}
	newArray := RemoveItemFromArray(array, remove)

	if array[0] != HexToAddress("0x0000003") || array[2] != HexToAddress("0x0000002") {
		t.Error("should keep the original item from array address")
	}
	if len(newArray) != 1 {
		t.Error("fail remove item from array address")
	}
}

var testCases = []struct {
	bin Address
	str string
}{
	{BlockSignersBinary, BlockSigners},
	{MasternodeVotingSMCBinary, MasternodeVotingSMC},
	{RandomizeSMCBinary, RandomizeSMC},
	{FoudationAddrBinary, FoudationAddr},
	{TeamAddrBinary, TeamAddr},
	{XDCXAddrBinary, XDCXAddr},
	{TradingStateAddrBinary, TradingStateAddr},
	{XDCXLendingAddressBinary, XDCXLendingAddress},
	{XDCXLendingFinalizedTradeAddressBinary, XDCXLendingFinalizedTradeAddress},
	{XDCNativeAddressBinary, XDCNativeAddress},
	{LendingLockAddressBinary, LendingLockAddress},
}

func TestBinaryAddressToString(t *testing.T) {
	for _, tt := range testCases {
		have := tt.bin.String()
		want := tt.str
		if have != want {
			t.Errorf("fail to convert binary address to string address\nwant:%s\nhave:%s", have, want)
		}
	}
}
func TestStringToBinaryAddress(t *testing.T) {
	for _, tt := range testCases {
		want := tt.bin
		have := HexToAddress(tt.str)
		if have != want {
			t.Errorf("fail to convert string address to binary address\nwant:%s\nhave:%s", have, want)
		}
	}
}
