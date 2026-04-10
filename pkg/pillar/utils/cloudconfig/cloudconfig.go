package cloudconfig

import (
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/lf-edge/eve/pkg/pillar/base"
	fileutils "github.com/lf-edge/eve/pkg/pillar/utils/file"
	"gopkg.in/yaml.v2"
)

// EveConfig holds EVE-specific settings embedded in a cloud-config file under
// the "eve_config" key. These settings are consumed by EVE before the VM is
// launched and are invisible to the guest's cloud-init implementation
// (unknown top-level keys are silently ignored by standard cloud-init).
type EveConfig struct {
	// DisableVMX disables Intel VMX (nested virtualisation) for the VM by
	// appending ",-vmx" to the QEMU -cpu argument. Has no effect on non-x86
	// architectures or on Xen/FML domains.
	DisableVMX bool `yaml:"disable_vmx"`
}

// CloudConfig represents the structure of the cloud configuration file.
// Only supported fields are defined here. The rest is ignored.
type CloudConfig struct {
	RunCmd     []string       `yaml:"runcmd"`
	WriteFiles []WritableFile `yaml:"write_files" validate:"dive"` //nolint:tagliatelle // cloud-init standard uses snake_case
	EveConfig  EveConfig      `yaml:"eve_config"`
}

// WritableFile represents a file that can be written to disk with the specified content, permissions, encoding and owner.
type WritableFile struct {
	Path        string `yaml:"path" validate:"required"`
	Content     string `yaml:"content" validate:"required"`
	Permissions string `yaml:"permissions"`
	Encoding    string `yaml:"encoding"`
	Owner       string `yaml:"owner"`
}

var validate = validator.New()

// MaxDecompressedContentSize is the maximum size of a file that can be written to disk after decompression.
// This is to prevent a DoS attack by sending a compressed file that is too big to be decompressed.
const MaxDecompressedContentSize = 10 * 1024 * 1024 // 10 MB

// IsCloudConfig checks if the given string is a cloud-config file by checking if the first line starts with "#cloud-config".
// It returns true if the first line starts with "#cloud-config", otherwise it returns false.
func IsCloudConfig(ci string) bool {
	// check if the first line is #cloud-config
	lines := strings.Split(ci, "\n")
	if len(lines) == 0 {
		return false
	}
	return strings.HasPrefix(lines[0], "#cloud-config")
}

// ParseCloudConfig parses the given cloud-init configuration and returns a pointer to a CloudConfig struct and an error.
func ParseCloudConfig(ci string) (*CloudConfig, error) {
	var cc CloudConfig
	err := yaml.Unmarshal([]byte(ci), &cc)
	if err != nil {
		return nil, err
	}
	err = validate.Struct(cc)
	if err != nil {
		return nil, err
	}
	return &cc, nil
}

// StripEveConfig removes the "eve_config" key from a cloud-config string and
// returns the sanitised YAML. This prevents EVE-specific directives from
// reaching the guest's cloud-init, which would otherwise log warnings (or fail
// in strict mode) for the unknown key.
// The leading "#cloud-config" header is preserved.
func StripEveConfig(ci string) (string, error) {
	var raw map[string]interface{}
	if err := yaml.Unmarshal([]byte(ci), &raw); err != nil {
		return "", err
	}
	delete(raw, "eve_config")
	out, err := yaml.Marshal(raw)
	if err != nil {
		return "", err
	}
	// Preserve the mandatory "#cloud-config" first line.
	return "#cloud-config\n" + string(out), nil
}

// WriteFile checks the content of a WritableFile and writes it to the specified rootPath.
func WriteFile(log *base.LogObject, file WritableFile, rootPath string) error {
	// transform file.Permission to os.FileMode
	perm, err := strconv.ParseUint(file.Permissions, 8, 32)
	if err != nil {
		return err
	}
	mode := os.FileMode(perm)

	writePath := filepath.Join(rootPath, file.Path)
	// sanitize path
	if !strings.HasPrefix(filepath.Clean(writePath), rootPath) {
		return fmt.Errorf("detected possible attempt to write file outside of root path. invalid path %s", file.Path)
	}

	var contentBytes []byte
	switch file.Encoding {
	case "b64", "base64":
		contentBytes, err = base64.StdEncoding.DecodeString(file.Content)

	case "plain", "":
		contentBytes = []byte(file.Content)

	case "gz+base64", "gzip+base64", "gz+b64", "gzip+b64":
		contentBytes, err = decodeGzipBase64Content(file.Content)

	case "gzip", "gz":
		contentBytes, err = decodeGzipContent(file.Content)

	default:
		return errors.New("unsupported encoding type. Only base64, plain and gzip are supported")
	}

	if err != nil {
		return err
	}

	// check if the parent directory exists
	parentDir := filepath.Dir(writePath)
	if _, err := os.Stat(parentDir); os.IsNotExist(err) {
		// create parent directory
		err = os.MkdirAll(parentDir, 0755)
		if err != nil {
			return err
		}
	}

	log.Tracef("Creating file %s with mode %s in %s\n", file.Path, mode, rootPath)
	err = fileutils.WriteRename(writePath, contentBytes)
	if err != nil {
		return err
	}
	err = os.Chmod(writePath, mode)
	if err != nil {
		return err
	}
	if file.Owner != "" {
		log.Warn("Changing owner of files written by cloud-init is not supported yet")
	}

	return nil
}

func decodeGzipContent(content string) ([]byte, error) {
	gzipReader, err := gzip.NewReader(strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	contentBytes, err := io.ReadAll(io.LimitReader(gzipReader, MaxDecompressedContentSize+1))
	if err != nil {
		return nil, err
	}
	if len(contentBytes) > MaxDecompressedContentSize {
		return nil, errors.New("maximum decompressed content size exceeded")
	}
	return contentBytes, nil
}

func decodeGzipBase64Content(content string) ([]byte, error) {
	gzipBytes, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, err
	}
	return decodeGzipContent(string(gzipBytes))
}
