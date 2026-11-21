package cppbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bishopfox/sliver/protobuf/clientpb"
)

// testImplantConfig returns a minimal but complete implant config as the RPC
// layer would hand it over.
func testImplantConfig(t *testing.T, format clientpb.OutputFormat) (*clientpb.ImplantConfig, *clientpb.ImplantBuild) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
			t.Skipf("unsupported windows arch %s", runtime.GOARCH)
		}
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skipf("unsupported host %s", runtime.GOOS)
	}
	config := &clientpb.ImplantConfig{
		ID:                "test-config",
		GOOS:              runtime.GOOS,
		GOARCH:            runtime.GOARCH,
		Format:            format,
		IncludeMTLS:       true,
		ReconnectInterval: 60 * 1e9,
		BeaconInterval:    60 * 1e9,
		C2: []*clientpb.ImplantC2{
			{URL: "mtls://127.0.0.1:8888"},
		},
		Exports: []string{"StartW"},
	}
	build := &clientpb.ImplantBuild{
		ID:                     "test-build",
		Name:                   "cppbuild-test",
		PeerPublicKey:          "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		PeerPrivateKey:         "AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ",
		PeerPublicKeySignature: "untrusted comment: test\nAAAA",
		AgeServerPublicKey:     "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		MinisignServerPublicKey: "untrusted comment: test\nAAAA",
		MtlsCACert:             "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
		MtlsCert:               "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
		MtlsKey:                "-----BEGIN EC PRIVATE KEY-----\nAA==\n-----END EC PRIVATE KEY-----\n",
	}
	return config, build
}

func requireCmake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not in PATH")
	}
	src, err := filepath.Abs(filepath.Join("..", "..", "implant-cpp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "CMakeLists.txt")); err != nil {
		t.Skipf("implant-cpp not checked out at %s", src)
	}
}

// TestCompileExecutable drives the whole cmake backend: config.json rendering,
// configure/build, artifact collection, then the PE/static/size gates.
func TestCompileExecutable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cmake build in -short mode")
	}
	requireCmake(t)
	tmp := t.TempDir()
	t.Setenv(envSliversDir, tmp)

	config, build := testImplantConfig(t, clientpb.OutputFormat_EXECUTABLE)
	path, err := CompileExecutable("cppbuild-test", build, config, nil)
	if err != nil {
		t.Fatalf("CompileExecutable: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	if info.Size() == 0 || info.Size() > MaxImplantSizeBytes {
		t.Fatalf("artifact size %d out of bounds", info.Size())
	}
	// config.json must exist next to the artifact and contain the C2 endpoint.
	cfgPath := filepath.Join(tmp, runtime.GOOS, runtime.GOARCH, "cppbuild-test", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config.json not written: %v", err)
	}
	if !contains(string(raw), "mtls://127.0.0.1:8888") {
		t.Fatalf("config.json lost the c2 endpoint:\n%s", raw)
	}
	t.Logf("artifact: %s (%d bytes)", path, info.Size())
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
