package client

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torob/topolab/internal/config"
)

const twoTemplateTopology = `name: lab
templates:
  - {name: uploaded, path: %s}
  - {name: onhost, path: /nonexistent/on/the/controller.qcow2, remote: true}
storages:
  - {name: local, type: file, root_provisioning: snapshot}
vms:
  - name: a
    template: uploaded
    root_disk: {storage: local}
    ssh_authorized_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"]
    interfaces: [{name: eth0}]
  - name: b
    template: onhost
    root_disk: {storage: local}
    ssh_authorized_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"]
    interfaces: [{name: eth0}]
switches: [{name: lan}]
links:
  - name: a-lan
    endpoints:
      - {node: a, interface: eth0}
      - {node: lan, interface: a}
  - name: b-lan
    endpoints:
      - {node: b, interface: eth0}
      - {node: lan, interface: b}
`

func TestPrepareTemplatesHashesOnlyPathTemplatesAndNeverReadsHostFiles(t *testing.T) {
	image := make([]byte, 64)
	copy(image, "QFI\xfb")
	binary.BigEndian.PutUint32(image[4:8], 3)
	file := filepath.Join(t.TempDir(), "uploaded.qcow2")
	if err := os.WriteFile(file, image, 0o644); err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(twoTemplateTopology, "%s", file, 1)
	cfg, _, err := config.Parse([]byte(doc), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{DigestCache: &DigestCache{}}
	inv := Invocation{Topology: cfg, Stderr: io.Discard}
	files, err := r.prepareTemplates(&inv)
	if err != nil {
		t.Fatalf("a remote template file that does not exist here made the controller fail: %v", err)
	}
	sum := sha256.Sum256(image)
	digest := hex.EncodeToString(sum[:])
	if len(inv.Templates) != 1 || inv.Templates[0].Name != "uploaded" || inv.Templates[0].SHA256 != digest || files[digest] != file {
		t.Fatalf("templates = %+v, files = %v", inv.Templates, files)
	}
}

func TestPrepareTemplatesListsNothingWhenEveryTemplateIsOnTheHost(t *testing.T) {
	doc := `name: lab
templates:
  - {name: onhost, path: /srv/images/base.qcow2, remote: true}
storages:
  - {name: local, type: file, root_provisioning: snapshot}
vms:
  - name: b
    template: onhost
    root_disk: {storage: local}
    ssh_authorized_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"]
    interfaces: [{name: eth0}]
switches: [{name: lan}]
links:
  - name: b-lan
    endpoints:
      - {node: b, interface: eth0}
      - {node: lan, interface: b}
`
	cfg, _, err := config.Parse([]byte(doc), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A nil cache would panic if the controller tried to hash the file.
	r := &Runner{}
	inv := Invocation{Topology: cfg, Stderr: io.Discard}
	files, err := r.prepareTemplates(&inv)
	if err != nil || len(inv.Templates) != 0 || len(files) != 0 {
		t.Fatalf("templates = %+v, files = %v, err = %v", inv.Templates, files, err)
	}
}
