// Package installstate is the saved answers of `rafiki setup` and `rafiki
// executor setup`: what reinstall and update replay. Non-secret only; secrets
// live in service.env / executor.env. Its keys are the future [install] section
// of rafikid.toml, so a key is never renamed.
package installstate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/paths"
)

// FileName is the leaf name of the install-state document under ConfigDir.
const FileName = "install.toml"

// SchemaVersion is the version this build reads and writes. Load refuses any
// other version, so a key's meaning can never be silently reinterpreted.
const SchemaVersion = 1

// Path is the install-state file: <ConfigDir>/install.toml.
func Path() string { return filepath.Join(paths.ConfigDir(), FileName) }

// Channel records how this rafiki was installed.
type Channel string

const (
	ChannelUnset   Channel = ""
	ChannelBrew    Channel = "brew"
	ChannelTarball Channel = "tarball"
	ChannelSource  Channel = "source"
)

// DBMode records how the database is provided.
type DBMode string

const (
	DBModeUnset   DBMode = ""
	DBModeManaged DBMode = "managed"
	DBModeDSN     DBMode = "dsn"
)

// ExecutorConnect records how the daemon reaches its executor.
type ExecutorConnect string

const (
	ExecutorConnectUnset  ExecutorConnect = ""
	ExecutorConnectSocket ExecutorConnect = "socket"
	ExecutorConnectTCP    ExecutorConnect = "tcp"
)

// State is the whole install-state document.
type State struct {
	Version  int      `toml:"version"`
	Channel  Channel  `toml:"channel"`
	LastStep string   `toml:"last_step"`
	DB       DB       `toml:"db"`
	Daemon   Daemon   `toml:"daemon"`
	Identity Identity `toml:"identity"`
	Executor Executor `toml:"executor"`
	Optional []string `toml:"optional"`
}

// DB is the [db] section.
type DB struct {
	Mode      DBMode `toml:"mode"`
	Container string `toml:"container"`
	Volume    string `toml:"volume"`
	Port      int    `toml:"port"`
	Image     string `toml:"image"`
}

// Daemon is the [daemon] section.
type Daemon struct {
	// Expose is a *bool: nil = never asked, which must not read as "no".
	Expose *bool `toml:"expose"`
}

// Identity is the [identity] section.
type Identity struct {
	User    string `toml:"user"`
	Profile string `toml:"profile"`
}

// Executor is the [executor] section.
type Executor struct {
	Connect    ExecutorConnect `toml:"connect"`
	Address    string          `toml:"address"`     // host:port, tcp only
	PinCert    string          `toml:"pin_cert"`    // tcp only
	ServerName string          `toml:"server_name"` // tcp only
	Root       string          `toml:"root"`
	Launch     []string        `toml:"launch"`
	Env        []string        `toml:"env"` // NAMES only; values are re-read at replay
	RTK        string          `toml:"rtk"`
}

// Load reads Path(). A missing file is not an error and reports ok == false. A
// decode leaves nothing undecoded — a mistyped key must not silently become an
// "unset" field — and a version other than SchemaVersion is refused.
func Load() (State, bool, error) {
	data, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, err
	}

	var s State
	md, err := toml.Decode(string(data), &s)
	if err != nil {
		return State{}, false, fmt.Errorf("%s: %w", FileName, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return State{}, false, fmt.Errorf("%s: unknown key(s): %s", FileName, strings.Join(keys, ", "))
	}
	if s.Version == 0 || s.Version > SchemaVersion {
		return State{}, false, fmt.Errorf("%s: unsupported version %d (this rafiki understands %d)", FileName, s.Version, SchemaVersion)
	}
	if err := s.Validate(); err != nil {
		return State{}, false, err
	}
	return s, true, nil
}

// Validate checks every enum against its constants (including the unset one)
// and bounds DB.Port.
func (s State) Validate() error {
	if err := validateEnum("channel", string(s.Channel), []string{string(ChannelUnset), string(ChannelBrew), string(ChannelTarball), string(ChannelSource)}); err != nil {
		return err
	}
	if err := validateEnum("db.mode", string(s.DB.Mode), []string{string(DBModeUnset), string(DBModeManaged), string(DBModeDSN)}); err != nil {
		return err
	}
	if err := validateEnum("executor.connect", string(s.Executor.Connect), []string{string(ExecutorConnectUnset), string(ExecutorConnectSocket), string(ExecutorConnectTCP)}); err != nil {
		return err
	}
	if s.DB.Port < 0 || s.DB.Port > 65535 {
		return fmt.Errorf("%s: db.port %d is not 0 or 1..65535", FileName, s.DB.Port)
	}
	return nil
}

// validateEnum reports field as invalid unless value is one of allowed. The
// error names only the meaningful (non-unset) constants, matching the message a
// person sees when they mistype a value.
func validateEnum(field, value string, allowed []string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	var names []string
	for _, a := range allowed {
		if a != "" {
			names = append(names, a)
		}
	}
	return fmt.Errorf("%s: %s %q is not one of %s", FileName, field, value, strings.Join(names, ", "))
}

// Save writes s atomically with Version forced to SchemaVersion. The temp file
// lives beside the destination so the rename is same-filesystem, and on any
// error it is removed so a failed write leaves no litter.
func Save(s State) (err error) {
	s.Version = SchemaVersion

	path := Path()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".install.toml-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err = toml.NewEncoder(f).Encode(s); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// tarballVersion matches the release tags a tarball install carries, e.g.
// v0.1.0. A git-describe suffix (-3-gabc) or a raw hash is not a release, so it
// reads as a source build.
var tarballVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// DetectChannel classifies an installation from the resolved executable path
// and the reported version. exe must be the resolved path (callers pass
// filepath.EvalSymlinks(os.Executable())) — a Homebrew symlink in bin/ would
// otherwise hide the Cellar it points into.
func DetectChannel(exe, version string) Channel {
	if strings.Contains(exe, "/Cellar/") || strings.Contains(exe, "/Caskroom/") {
		return ChannelBrew
	}
	if tarballVersion.MatchString(version) {
		return ChannelTarball
	}
	return ChannelSource
}
