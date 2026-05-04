// Command encrypt-calltypes manages the confidential call-types file used by the transcribe
// service. It is a small operator tool, not part of the runtime path.
//
// Subcommands:
//
//	generate-key                          # prints a fresh hex-encoded 32-byte key to stdout
//	encrypt -in IN -out OUT [-key KEY]    # encrypts a plaintext list to OUT
//	decrypt -in IN -out OUT [-key KEY]    # decrypts an encrypted list to OUT
//
// The key may be provided via the -key flag or the CALL_TYPES_KEY env var. Plaintext files
// are one call type per line; blank lines and surrounding whitespace are ignored at runtime.
//
// Generated keys are written to stdout only; everything diagnostic goes to stderr so the
// output can be piped or captured cleanly.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/searchandrescuegg/transcribe/internal/calltypes"
)

const keyEnvVar = "CALL_TYPES_KEY"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "generate-key":
		runGenerateKey()
	case "encrypt":
		runEncrypt(os.Args[2:])
	case "decrypt":
		runDecrypt(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `encrypt-calltypes — manage the confidential call-types file.

Usage:
  encrypt-calltypes generate-key
  encrypt-calltypes encrypt -in PATH -out PATH [-key HEX]
  encrypt-calltypes decrypt -in PATH -out PATH [-key HEX]

Flags:
  -key HEX   hex-encoded 32-byte AES key (default: $%s)

Examples:
  # Generate a key and stash it where your secret manager can find it.
  encrypt-calltypes generate-key > /tmp/call_types.key
  export %s=$(cat /tmp/call_types.key)

  # Encrypt a plaintext list (one call type per line) into the committed file.
  encrypt-calltypes encrypt -in config/call_types.txt -out config/call_types.enc

  # Decrypt for editing, then re-encrypt and commit.
  encrypt-calltypes decrypt -in config/call_types.enc -out /tmp/call_types.txt
  $EDITOR /tmp/call_types.txt
  encrypt-calltypes encrypt -in /tmp/call_types.txt -out config/call_types.enc
`, keyEnvVar, keyEnvVar)
}

func runGenerateKey() {
	hexKey, err := calltypes.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to generate key:", err)
		os.Exit(1)
	}
	fmt.Println(hexKey)
}

func runEncrypt(args []string) {
	in, out, hexKey := parseEncryptDecryptFlags("encrypt", args)
	key, err := calltypes.ParseKey(hexKey)
	if err != nil {
		fatalf("invalid key: %v", err)
	}

	plaintext, err := os.ReadFile(in)
	if err != nil {
		fatalf("read %s: %v", in, err)
	}
	blob, err := calltypes.Encrypt(plaintext, key)
	if err != nil {
		fatalf("encrypt: %v", err)
	}
	// 0o600 because even the ciphertext shouldn't be world-readable on a shared host.
	if err := os.WriteFile(out, blob, 0o600); err != nil {
		fatalf("write %s: %v", out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes of ciphertext to %s\n", len(blob), out)
}

func runDecrypt(args []string) {
	in, out, hexKey := parseEncryptDecryptFlags("decrypt", args)
	key, err := calltypes.ParseKey(hexKey)
	if err != nil {
		fatalf("invalid key: %v", err)
	}

	blob, err := os.ReadFile(in)
	if err != nil {
		fatalf("read %s: %v", in, err)
	}
	plaintext, err := calltypes.Decrypt(blob, key)
	if err != nil {
		fatalf("decrypt: %v", err)
	}
	// 0o600 because plaintext is the secret we're protecting; never widen this.
	if err := os.WriteFile(out, plaintext, 0o600); err != nil {
		fatalf("write %s: %v", out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote plaintext to %s (mode 0600)\n", out)
}

func parseEncryptDecryptFlags(name string, args []string) (in, out, hexKey string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&in, "in", "", "input file path (required)")
	fs.StringVar(&out, "out", "", "output file path (required)")
	fs.StringVar(&hexKey, "key", os.Getenv(keyEnvVar), "hex-encoded 32-byte AES key (default: $"+keyEnvVar+")")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if in == "" || out == "" {
		fmt.Fprintln(os.Stderr, "both -in and -out are required")
		fs.Usage()
		os.Exit(2)
	}
	if hexKey == "" {
		fmt.Fprintf(os.Stderr, "key not provided; set $%s or pass -key\n", keyEnvVar)
		os.Exit(2)
	}
	return in, out, hexKey
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
