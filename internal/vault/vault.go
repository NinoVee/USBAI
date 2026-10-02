// Package vault stores the user's data on the drive encrypted with a key
// derived from their passphrase. Without the passphrase a stolen drive
// reveals only random-looking files.
//
// Format: <dir>/vault.json holds the KDF salt and parameters plus an
// encrypted check value. Each object is stored in <dir>/objects/<hmac-sha256(name)>
// as nonce || AES-256-GCM(ciphertext), with the object name bound in as
// additional data so files cannot be swapped undetected. Hashing names keeps
// document names, chat titles and IDs out of the file listing.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	headerFile    = "vault.json"
	objectsDir    = "objects"
	indexName     = "__index__"
	checkPlain    = "private-ai-vault-v1"
	kdfIterations = 600_000
	formatVersion = 1
)

// ErrWrongPassphrase is returned by Open for an incorrect passphrase.
var ErrWrongPassphrase = errors.New("wrong passphrase")

// ErrNotFound is returned by Get for a missing object.
var ErrNotFound = errors.New("not found")

type header struct {
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       []byte `json:"salt"`
	Check      []byte `json:"check"`
}

// Vault is an unlocked vault. It is safe for concurrent use.
type Vault struct {
	dir     string
	aead    cipher.AEAD
	nameKey []byte // HMAC key for object file names
	mu      sync.Mutex
	names   map[string]struct{} // plaintext object names, kept in the encrypted index
}

// Exists reports whether a vault has been created in dir.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, headerFile))
	return err == nil
}

// Create makes a new vault in dir. It fails if one already exists.
func Create(dir, passphrase string) (*Vault, error) {
	if Exists(dir) {
		return nil, errors.New("vault already exists")
	}
	if len(passphrase) < 8 {
		return nil, errors.New("passphrase must be at least 8 characters")
	}
	if err := os.MkdirAll(filepath.Join(dir, objectsDir), 0o700); err != nil {
		return nil, err
	}
	h := header{Version: formatVersion, KDF: "pbkdf2-sha256", Iterations: kdfIterations, Salt: make([]byte, 32)}
	if _, err := rand.Read(h.Salt); err != nil {
		return nil, err
	}
	v, err := newVault(dir, passphrase, h)
	if err != nil {
		return nil, err
	}
	h.Check = v.seal([]byte(checkPlain), headerFile)
	b, _ := json.MarshalIndent(h, "", "  ")
	if err := writeAtomic(filepath.Join(dir, headerFile), b); err != nil {
		return nil, err
	}
	if err := v.saveIndex(); err != nil {
		return nil, err
	}
	return v, nil
}

// Open unlocks an existing vault.
func Open(dir, passphrase string) (*Vault, error) {
	b, err := os.ReadFile(filepath.Join(dir, headerFile))
	if err != nil {
		return nil, err
	}
	var h header
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("corrupt vault header: %w", err)
	}
	if h.Version != formatVersion || h.KDF != "pbkdf2-sha256" {
		return nil, fmt.Errorf("unsupported vault format %d/%s", h.Version, h.KDF)
	}
	v, err := newVault(dir, passphrase, h)
	if err != nil {
		return nil, err
	}
	plain, err := v.open(h.Check, headerFile)
	if err != nil || string(plain) != checkPlain {
		return nil, ErrWrongPassphrase
	}
	idx, err := v.Get(indexName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err == nil {
		var names []string
		if err := json.Unmarshal(idx, &names); err != nil {
			return nil, fmt.Errorf("corrupt vault index: %w", err)
		}
		for _, n := range names {
			v.names[n] = struct{}{}
		}
	}
	return v, nil
}

// newVault derives 64 bytes from the passphrase: the first half is the
// AES-256 key, the second half keys the HMAC that names object files.
func newVault(dir, passphrase string, h header) (*Vault, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, h.Salt, h.Iterations, 64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{dir: dir, aead: aead, nameKey: key[32:], names: map[string]struct{}{}}, nil
}

func (v *Vault) seal(plain []byte, name string) []byte {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return v.aead.Seal(nonce, nonce, plain, []byte(name))
}

func (v *Vault) open(sealed []byte, name string) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext too short")
	}
	return v.aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
}

func (v *Vault) objectPath(name string) string {
	mac := hmac.New(sha256.New, v.nameKey)
	mac.Write([]byte(name))
	return filepath.Join(v.dir, objectsDir, hex.EncodeToString(mac.Sum(nil)))
}

// Put stores data under name, replacing any previous value.
func (v *Vault) Put(name string, data []byte) error {
	if name == "" || name == indexName {
		return errors.New("invalid object name")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := writeAtomic(v.objectPath(name), v.seal(data, name)); err != nil {
		return err
	}
	if _, ok := v.names[name]; !ok {
		v.names[name] = struct{}{}
		return v.saveIndex()
	}
	return nil
}

// Get returns the data stored under name.
func (v *Vault) Get(name string) ([]byte, error) {
	b, err := os.ReadFile(v.objectPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := v.open(b, name)
	if err != nil {
		return nil, fmt.Errorf("object %q failed integrity check: %w", name, err)
	}
	return plain, nil
}

// Delete removes name. Deleting a missing object is not an error.
func (v *Vault) Delete(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := os.Remove(v.objectPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, ok := v.names[name]; ok {
		delete(v.names, name)
		return v.saveIndex()
	}
	return nil
}

// List returns the stored names starting with prefix, sorted.
func (v *Vault) List(prefix string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for n := range v.names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// PutJSON and GetJSON are helpers for structured objects.
func (v *Vault) PutJSON(name string, x any) error {
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	return v.Put(name, b)
}

func (v *Vault) GetJSON(name string, x any) error {
	b, err := v.Get(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, x)
}

// saveIndex must be called with v.mu held (or before v is shared).
func (v *Vault) saveIndex() error {
	names := make([]string, 0, len(v.names))
	for n := range v.names {
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(names)
	return writeAtomic(v.objectPath(indexName), v.seal(b, indexName))
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// Flush to the device: USB drives are often pulled without ejecting.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
