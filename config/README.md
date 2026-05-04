# config

Runtime configuration files. Only the encrypted call-types blob should ever be committed.

## Files

| File | Committed? | Purpose |
| --- | --- | --- |
| `call_types.example.txt` | yes | Sanitized illustration of the plaintext format. Not used at runtime. |
| `call_types.enc` | yes (when present) | AES-256-GCM ciphertext of the real call-types list. Loaded at startup when `CALL_TYPES_PATH` points at it. |
| `call_types.txt` | **no** (gitignored) | Plaintext you author and decrypt to locally. Never commit. |

## Workflow

```bash
# One-time setup: generate a key, store it where your secret manager lives.
go run ./cmd/encrypt-calltypes generate-key > /tmp/call_types.key
export CALL_TYPES_KEY=$(cat /tmp/call_types.key)

# Author the list locally (gitignored path).
$EDITOR config/call_types.txt

# Encrypt and commit.
go run ./cmd/encrypt-calltypes encrypt -in config/call_types.txt -out config/call_types.enc
git add config/call_types.enc
git commit -m "chore: rotate call-types list"

# Wipe the plaintext after you're done.
rm config/call_types.txt
```

To edit later, decrypt → edit → re-encrypt:

```bash
go run ./cmd/encrypt-calltypes decrypt -in config/call_types.enc -out config/call_types.txt
$EDITOR config/call_types.txt
go run ./cmd/encrypt-calltypes encrypt -in config/call_types.txt -out config/call_types.enc
rm config/call_types.txt
```

## At runtime

```bash
CALL_TYPES_PATH=config/call_types.enc \
CALL_TYPES_KEY=<hex-encoded-32-byte-key> \
./transcribe
```

When `CALL_TYPES_PATH` is set, the service:

1. Loads + decrypts the file at startup. Wrong key or tampered ciphertext fails the boot.
2. Inlines the list into the OpenAI system prompt so the model knows the canonical spelling.
3. Adds an `enum` constraint on `call_type` in the structured-output JSON schema with
   `Strict: true`, so the model can only emit values from the list (plus `Unknown`).

Leaving `CALL_TYPES_PATH` empty disables the constraint and the service uses the in-prompt
example call types.
