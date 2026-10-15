package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

type hasher struct{ h hash.Hash }

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (w *hasher) Write(b []byte) { _, _ = w.h.Write(b) }

func (w *hasher) Sum() string { return hex.EncodeToString(w.h.Sum(nil)) }
