package types

import (
	"encoding/json"
	"testing"

	cbor "gx/ipfs/QmRoARq3nkUb13HSKZGepCZSWe5GrVPwx7xURJGZ7KWv9V/go-ipld-cbor"

	"github.com/stretchr/testify/assert"
)

func TestUint64CBor(t *testing.T) {
	assert := assert.New(t)

	v := Uint64(64)
	m, err := cbor.DumpObject(v)
	assert.NoError(err)
	var got Uint64
	err = cbor.DecodeInto(m, &got)
	assert.NoError(err)
	assert.Equal(v, got)
}

func TestUint64Json(t *testing.T) {
	assert := assert.New(t)

	v := Uint64(64)
	m, err := json.Marshal(v)
	assert.NoError(err)
	var got Uint64
	err = json.Unmarshal(m, &got)
	assert.NoError(err)
	assert.Equal(v, got)
}
