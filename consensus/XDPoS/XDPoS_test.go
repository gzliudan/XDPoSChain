package XDPoS

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func TestGetM1M2FromCheckpointHeader(t *testing.T) {
	masternodes := []common.Address{
		common.BytesToAddress([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
		common.BytesToAddress([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")),
		common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc")),
	}
	validators := []int64{
		2,
		1,
		0,
	}
	epoch := uint64(900)
	config := &params.ChainConfig{
		XDPoS: &params.XDPoSConfig{
			Epoch: uint64(epoch),
		},
	}
	testMoveM2 := []uint64{0, 0, 0, 1, 1, 1, 2, 2, 2, 0, 0, 0, 1, 1, 1, 2, 2, 2}
	//try from block 3410001 to 3410018
	for i := uint64(3464001); i <= 3464018; i++ {
		currentNumber := int64(i)
		currentHeader := &types.Header{
			Number: big.NewInt(currentNumber),
		}
		m1m2, moveM2, err := getM1M2(masternodes, validators, currentHeader, config)
		if err != nil {
			t.Error("can't get m1m2", "err", err)
		}
		fmt.Printf("block: %v, moveM2: %v\n", currentHeader.Number.Int64(), moveM2)
		for _, k := range masternodes {
			fmt.Printf("m1: %v - m2: %v\n", k.String(), m1m2[k].String())
		}
		if moveM2 != testMoveM2[i-3464001] {
			t.Error("wrong moveM2", "currentNumber", currentNumber, "want", testMoveM2[i-3464001], "have", moveM2)
		}
	}
}

func TestCompareSignersLists(t *testing.T) {
	list1 := []common.Address{
		common.BytesToAddress([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
		common.BytesToAddress([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")),
		common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc")),
		common.BytesToAddress([]byte("dddddddddddddddddddddddddddddddddddddddd")),
	}
	list2 := []common.Address{
		common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc")),
		common.BytesToAddress([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")),
		common.BytesToAddress([]byte("dddddddddddddddddddddddddddddddddddddddd")),
		common.BytesToAddress([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")),
	}
	list3 := []common.Address{
		common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc")),
		common.BytesToAddress([]byte("dddddddddddddddddddddddddddddddddddddddd")),
		common.BytesToAddress([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")),
	}
	if !compareSignersLists(list1, list2) {
		t.Error("list1 should be equal to list2", "list1", list1, "list2", list2)
	}
	if compareSignersLists(list1, list3) {
		t.Error("list1 and list3 should not be same", "list1", list1, "list3", list3)
	}
	if !compareSignersLists([]common.Address{}, []common.Address{}) {
		t.Error("Failed with empty list")
	}
	if !compareSignersLists([]common.Address{common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc"))},
		[]common.Address{common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc"))}) {
		t.Error("Failed with list has only one signer")
	}
	if compareSignersLists([]common.Address{common.BytesToAddress([]byte("aaaaaaaaaaaaaaaa"))},
		[]common.Address{common.BytesToAddress([]byte("cccccccccccccccccccccccccccccccccccccccc"))}) {
		t.Error("Failed with list has only one signer")
	}
}
