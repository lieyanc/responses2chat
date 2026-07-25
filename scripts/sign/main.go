// Command sign signs release assets with the Ed25519 update signing key.
//
// Usage:
//
//	go run ./scripts/sign -genkey
//	    Generate a new key pair; prints the seed (keep secret) and public key.
//	go run ./scripts/sign -pubkey
//	    Print the public key derived from UPDATE_SIGNING_KEY.
//	UPDATE_SIGNING_KEY=<hex seed> go run ./scripts/sign FILE...
//	    Write a base64 Ed25519 signature of each FILE to FILE.sig.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	genkey := flag.Bool("genkey", false, "generate a new key pair and exit")
	pubkey := flag.Bool("pubkey", false, "print the public key for UPDATE_SIGNING_KEY and exit")
	flag.Parse()

	if *genkey {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			fatalf("generate seed: %v", err)
		}
		key := ed25519.NewKeyFromSeed(seed)
		fmt.Printf("UPDATE_SIGNING_KEY (secret, hex seed): %s\n", hex.EncodeToString(seed))
		fmt.Printf("public key (embed in updater):        %s\n", hex.EncodeToString(key.Public().(ed25519.PublicKey)))
		return
	}

	key := loadKey()
	if *pubkey {
		fmt.Println(hex.EncodeToString(key.Public().(ed25519.PublicKey)))
		return
	}

	if flag.NArg() == 0 {
		fatalf("no files to sign; pass file paths as arguments")
	}
	for _, path := range flag.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			fatalf("read %s: %v", path, err)
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))
		if err := os.WriteFile(path+".sig", []byte(sig+"\n"), 0o644); err != nil {
			fatalf("write %s.sig: %v", path, err)
		}
		fmt.Printf("signed %s\n", path)
	}
}

func loadKey() ed25519.PrivateKey {
	seedHex := strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY"))
	if seedHex == "" {
		fatalf("UPDATE_SIGNING_KEY is not set")
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		fatalf("UPDATE_SIGNING_KEY must be a hex-encoded %d-byte seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
