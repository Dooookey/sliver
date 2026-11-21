package cppbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bishopfox/sliver/protobuf/clientpb"
)

// buildConfig is the JSON contract between the Go server and the C/C++ implant
// build (consumed by implant-cpp/cmake/SliverConfigGen.cmake). Field names are
// stable: they are part of the generator<->template contract, exactly like the
// Go text/template placeholders were.
type buildConfig struct {
	Name              string      `json:"name"`
	BuildID           string      `json:"build_id"`
	Strategy          string      `json:"strategy"`
	Beacon            bool        `json:"beacon"`
	ReconnectInterval int64       `json:"reconnect_interval_ns"`
	BeaconInterval    int64       `json:"beacon_interval_ns"`
	BeaconJitter      int64       `json:"beacon_jitter_ns"`
	MaxConnectionErrs int         `json:"max_connection_errors"`
	PollTimeout       int64       `json:"poll_timeout"`
	Features          features    `json:"features"`
	C2                []c2Entry   `json:"c2"`
	Limits            limits      `json:"limits"`
	Secrets           secrets     `json:"secrets"`
	HTTP              *httpImplant `json:"http,omitempty"`
}

type features struct {
	MTLS  bool `json:"mtls"`
	HTTP  bool `json:"http"`
	DNS   bool `json:"dns"`
	WG    bool `json:"wg"`
	Pivot bool `json:"pivot"`
}

type c2Entry struct {
	URL     string `json:"url"`
	Options string `json:"options,omitempty"`
}

type limits struct {
	DomainJoined bool   `json:"domain_joined"`
	Datetime     string `json:"datetime"`
	Hostname     string `json:"hostname"`
	Username     string `json:"username"`
	FileExists   string `json:"file_exists"`
	Locale       string `json:"locale"`
}

// secrets carries everything that must never appear in a header file.
type secrets struct {
	PeerPublicKey           string `json:"peer_public_key"`
	PeerPrivateKey          string `json:"peer_private_key"`
	PeerPublicKeySignature  string `json:"peer_public_key_signature"`
	AgeServerPublicKey      string `json:"age_server_public_key"`
	MinisignServerPublicKey string `json:"minisign_server_public_key"`
	MtlsCACert              string `json:"mtls_ca_cert"`
	MtlsCert                string `json:"mtls_cert"`
	MtlsKey                 string `json:"mtls_key"`
	WGImplantPrivKey        string `json:"wg_implant_priv_key"`
	WGServerPubKey          string `json:"wg_server_pub_key"`
	WGPeerTunIP             string `json:"wg_peer_tun_ip"`
}

type httpImplant struct {
	UserAgent          string        `json:"user_agent"`
	NonceQueryArgChars string        `json:"nonce_query_arg_chars"`
	NonceQueryArgLen   int32         `json:"nonce_query_arg_length"`
	NonceMode          string        `json:"nonce_mode"`
	Headers            []httpKV      `json:"headers"`
	URLParameters      []httpKV      `json:"url_parameters"`
	PathSegments       []pathSegment `json:"path_segments"`
	Extensions         []string      `json:"extensions"`
	MinPathLength      int32         `json:"min_path_length"`
	MaxPathLength      int32         `json:"max_path_length"`
	MinFileGen         int32         `json:"min_file_gen"`
	MaxFileGen         int32         `json:"max_file_gen"`
}

type httpKV struct {
	Method      string `json:"method"`
	Name        string `json:"name"`
	Value       string `json:"value"`
	Probability int32  `json:"probability"`
}

type pathSegment struct {
	IsFile bool   `json:"is_file"`
	Value  string `json:"value"`
}

// WriteConfigJSON renders the whole implant configuration to disk.
func WriteConfigJSON(path, name string, build *clientpb.ImplantBuild, config *clientpb.ImplantConfig, httpC2 *clientpb.HTTPC2ImplantConfig) error {
	cfg := buildConfig{
		Name:              name,
		BuildID:           firstNonEmpty(config.ID, build.ID, name),
		Strategy:          config.ConnectionStrategy,
		Beacon:            config.IsBeacon,
		ReconnectInterval: config.ReconnectInterval,
		BeaconInterval:    config.BeaconInterval,
		BeaconJitter:      config.BeaconJitter,
		MaxConnectionErrs: int(config.MaxConnectionErrors),
		PollTimeout:       config.PollTimeout,
		Features: features{
			MTLS:  config.IncludeMTLS,
			HTTP:  config.IncludeHTTP,
			DNS:   config.IncludeDNS,
			WG:    config.IncludeWG,
			Pivot: config.IncludeTCP || config.IncludeNamePipe,
		},
		Limits: limits{
			DomainJoined: config.LimitDomainJoined,
			Datetime:     config.LimitDatetime,
			Hostname:     config.LimitHostname,
			Username:     config.LimitUsername,
			FileExists:   config.LimitFileExists,
			Locale:       config.LimitLocale,
		},
		Secrets: secrets{
			PeerPublicKey:           build.PeerPublicKey,
			PeerPrivateKey:          build.PeerPrivateKey,
			PeerPublicKeySignature:  build.PeerPublicKeySignature,
			AgeServerPublicKey:      build.AgeServerPublicKey,
			MinisignServerPublicKey: build.MinisignServerPublicKey,
			MtlsCACert:              build.MtlsCACert,
			MtlsCert:                build.MtlsCert,
			MtlsKey:                 build.MtlsKey,
			WGImplantPrivKey:        build.WGImplantPrivKey,
			WGServerPubKey:          build.WGServerPubKey,
			WGPeerTunIP:             config.WGPeerTunIP,
		},
	}

	for _, c2 := range config.C2 {
		if c2 == nil || c2.URL == "" {
			continue
		}
		cfg.C2 = append(cfg.C2, c2Entry{URL: c2.URL, Options: c2.Options})
	}
	if len(cfg.C2) == 0 {
		return fmt.Errorf("cppbuild: implant config has no C2 endpoints")
	}
	// The C side embeds the strategy as a char literal; an empty value would
	// produce `''` and fail to compile.
	if cfg.Strategy == "" {
		cfg.Strategy = "s"
	}

	if httpC2 != nil && config.IncludeHTTP {
		cfg.HTTP = buildHTTPProfile(httpC2)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// firstNonEmpty returns the first non-empty string (build/config ids).
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func buildHTTPProfile(p *clientpb.HTTPC2ImplantConfig) *httpImplant {	out := &httpImplant{
		UserAgent:          p.UserAgent,
		NonceQueryArgChars: p.NonceQueryArgChars,
		NonceQueryArgLen:   p.NonceQueryLength,
		NonceMode:          p.NonceMode,
		Extensions:         p.Extensions,
		MinPathLength:      p.MinPathLength,
		MaxPathLength:      p.MaxPathLength,
		MinFileGen:         p.MinFileGen,
		MaxFileGen:         p.MaxFileGen,
	}
	for _, h := range p.Headers {
		if h == nil {
			continue
		}
		out.Headers = append(out.Headers, httpKV{
			Method: h.Method, Name: h.Name, Value: h.Value, Probability: h.Probability,
		})
	}
	for _, q := range p.ExtraURLParameters {
		if q == nil {
			continue
		}
		out.URLParameters = append(out.URLParameters, httpKV{
			Method: q.Method, Name: q.Name, Value: q.Value, Probability: q.Probability,
		})
	}
	for _, seg := range p.PathSegments {
		if seg == nil {
			continue
		}
		out.PathSegments = append(out.PathSegments, pathSegment{IsFile: seg.IsFile, Value: seg.Value})
	}
	return out
}
