package pep

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

func randomFlowID() (uint64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, fmt.Errorf("generate flow id: %w", err)
	}
	id := binary.BigEndian.Uint64(raw[:])
	if id == 0 {
		id = 1
	}
	return id, nil
}
