package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func newID(prefix string) string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return prefix + "_000000000000000000000000"
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}

func responseID(chatID string) string {
	if chatID == "" {
		return newID("resp")
	}
	if strings.HasPrefix(chatID, "chatcmpl-") {
		return "resp_" + strings.TrimPrefix(chatID, "chatcmpl-")
	}
	return "resp_" + chatID
}
