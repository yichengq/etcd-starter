// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package starter

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/coreos/etcd-starter/Godeps/_workspace/src/github.com/coreos/etcd/client"
	"github.com/coreos/etcd-starter/Godeps/_workspace/src/github.com/coreos/etcd/etcdmain"
	"github.com/coreos/etcd-starter/Godeps/_workspace/src/github.com/coreos/etcd/migrate"
	"github.com/coreos/etcd-starter/Godeps/_workspace/src/github.com/coreos/etcd/pkg/flags"
	"github.com/coreos/etcd-starter/Godeps/_workspace/src/github.com/coreos/etcd/pkg/osutil"
	"github.com/coreos/etcd-starter/Godeps/_workspace/src/github.com/coreos/etcd/wal"
	"github.com/coreos/etcd-starter/Godeps/_workspace/src/golang.org/x/net/context"
)

type version string

const (
	internalV1      version = "1"
	internalV2      version = "2"
	internalV2Proxy version = "2.proxy"
	internalUnknown version = "unknown"

	v0_4      version = "v0.4"
	v2_0      version = "v2.0"
	v2_0Proxy version = "v2.0 proxy"
	empty     version = "empty"
	unknown   version = "unknown"
)

var (
	v2SpecialFlags = []string{
		"initial-cluster",
		"listen-peer-urls",
		"listen-client-urls",
		"proxy",
	}
)

func StartDesiredVersion(binDir string, args []string) {
	fs, err := parseConfig(args)
	if err != nil {
		return
	}

	ver := checkInternalVersion(fs)
	fmt.Printf("etcd-starter: starting etcd version %s\n", ver)
	var p string
	switch ver {
	case internalV1:
		p = path.Join(binDir, "1", "etcd")
	case internalV2:
		p = path.Join(binDir, "2", "etcd")
	case internalV2Proxy:
		p = path.Join(binDir, "2", "etcd")
		if _, err := os.Stat(standbyInfo4(fs.Lookup("data-dir").Value.String())); err != nil {
			fmt.Printf("etcd-starter: detected standby_info file. Adding --proxy=on flag to ensure node runs in v2.0 proxy mode.\n")
			fmt.Printf("etcd-starter: before removing v0.4 data, --proxy=on flag MUST be added.\n")
		}
		// append proxy flag to args to trigger proxy mode
		args = append(args, "-proxy=on")
	default:
		log.Panicf("etcd-starter: unhandled start version")
	}

	fmt.Printf("etcd-starter: starting with %s %v with env %v\n", p, args, syscall.Environ())
	err = syscall.Exec(p, append([]string{p}, args...), syscall.Environ())
	if err != nil {
		log.Fatalf("etcd-starter: failed to execute %s: %v", p, err)
	}
}

func checkInternalVersion(fs *flag.FlagSet) version {
	// If it uses 2.0 env var explicitly, start 2.0
	for _, name := range v2SpecialFlags {
		if fs.Lookup(name).Value.String() != "" {
			return internalV2
		}
	}

	dataDir := fs.Lookup("data-dir").Value.String()
	if dataDir == "" {
		fmt.Printf("etcd-starter: data-dir is not set\n")
		return internalV2
	}
	// check the data directory
	dataver, err := wal.DetectVersion(dataDir)
	if err != nil {
		log.Fatalf("etcd-starter: failed to detect etcd version in %v: %v", dataDir, err)
	}
	fmt.Printf("etcd-starter: detected etcd version %s in %s\n", dataver, dataDir)
	switch dataver {
	case wal.WALv2_0:
		return internalV2
	case wal.WALv2_0_1:
		return internalV2
	case wal.WALv2_0Proxy:
		return internalV2Proxy
	case wal.WALv0_4:
		standbyInfo, err := migrate.DecodeStandbyInfo4FromFile(standbyInfo4(dataDir))
		if err != nil && !os.IsNotExist(err) {
			log.Printf("etcd-starter: failed to decode standbyInfo in %v: %v", dataDir, err)
			return internalV1
		}
		inStandbyMode := standbyInfo != nil && standbyInfo.Running
		if inStandbyMode {
			ver, err := checkInternalVersionByClientURLs(standbyInfo.ClientURLs(), clientTLSInfo(fs))
			if err != nil {
				fmt.Printf("etcd-starter: failed to check start version through peers: %v\n", err)
				return internalV1
			}
			if ver == internalV2 {
				osutil.Unsetenv("ETCD_DISCOVERY")
				os.Args = append(os.Args, "-initial-cluster", standbyInfo.InitialCluster())
				return internalV2Proxy
			}
			return ver
		}
		ver, err := checkInternalVersionByDataDir4(dataDir)
		if err != nil {
			log.Printf("etcd-starter: failed to check start version in %v: %v", dataDir, err)
			return internalV1
		}
		return ver
	case wal.WALNotExist:
		discovery := fs.Lookup("discovery").Value.String()
		dpeers, err := getPeersFromDiscoveryURL(discovery)
		if err != nil {
			fmt.Printf("etcd-starter: failed to get peers from discovery %s: %v\n", discovery, err)
		}
		peerStr := fs.Lookup("peers").Value.String()
		ppeers := getPeersFromPeersFlag(peerStr, peerTLSInfo(fs))

		urls := getClientURLsByPeerURLs(append(dpeers, ppeers...), peerTLSInfo(fs))
		ver, err := checkInternalVersionByClientURLs(urls, clientTLSInfo(fs))
		if err != nil {
			fmt.Printf("etcd-starter: failed to check start version through peers: %v\n", err)
			return internalV2
		}
		return ver
	case wal.WALUnknown:
		fmt.Printf("etcd-starter: unrecognized contents in data directory %s\n", dataDir)
		return internalV2
	}
	// never reach here
	log.Panicf("etcd-starter: unhandled etcd version in %v", dataDir)
	return internalUnknown
}

func checkInternalVersionByDataDir4(dataDir string) (version, error) {
	// check v0.4 snapshot
	snap4, err := migrate.DecodeLatestSnapshot4FromDir(snapDir4(dataDir))
	if err != nil {
		return internalUnknown, err
	}
	if snap4 != nil {
		st := &migrate.Store4{}
		if err := json.Unmarshal(snap4.State, st); err != nil {
			return internalUnknown, err
		}
		dir := st.Root.Children["_etcd"]
		n, ok := dir.Children["next-internal-version"]
		if ok && n.Value == "2" {
			return internalV2, nil
		}
	}

	// check v0.4 log
	ents4, err := migrate.DecodeLog4FromFile(logFile4(dataDir))
	if err != nil {
		return internalUnknown, err
	}
	for _, e := range ents4 {
		cmd, err := migrate.NewCommand4(e.GetCommandName(), e.GetCommand(), nil)
		if err != nil {
			return internalUnknown, err
		}
		setcmd, ok := cmd.(*migrate.SetCommand)
		if !ok {
			continue
		}
		if setcmd.Key == "/_etcd/next-internal-version" && setcmd.Value == "2" {
			return internalV2, nil
		}
	}
	return internalV1, nil
}

func getClientURLsByPeerURLs(peers []string, tls *TLSInfo) []string {
	c, err := newDefaultClient(tls)
	if err != nil {
		fmt.Printf("etcd-starter: new client error: %v\n", err)
		return nil
	}
	var urls []string
	for _, u := range peers {
		resp, err := c.Get(u + "/etcdURL")
		if err != nil {
			fmt.Printf("etcd-starter: failed to get /etcdURL from %s\n", u)
			continue
		}
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("etcd-starter: failed to read body from %s\n", u)
			continue
		}
		urls = append(urls, string(b))
	}
	return urls
}

func checkInternalVersionByClientURLs(urls []string, tls *TLSInfo) (version, error) {
	c, err := newDefaultClient(tls)
	if err != nil {
		return internalUnknown, err
	}
	for _, u := range urls {
		resp, err := c.Get(u + "/version")
		if err != nil {
			fmt.Printf("etcd-starter: failed to get /version from %s\n", u)
			continue
		}
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("etcd-starter: failed to read body from %s\n", u)
			continue
		}

		var m map[string]string
		err = json.Unmarshal(b, &m)
		if err != nil {
			fmt.Printf("etcd-starter: failed to unmarshal body %s from %s\n", b, u)
			continue
		}
		switch m["internalVersion"] {
		case "1":
			return internalV1, nil
		case "2":
			return internalV2, nil
		default:
			fmt.Printf("etcd-starter: unrecognized internal version %s from %s\n", m["internalVersion"], u)
		}
	}
	return internalUnknown, fmt.Errorf("failed to get version from urls %v", urls)
}

func getPeersFromDiscoveryURL(discoverURL string) ([]string, error) {
	if discoverURL == "" {
		return nil, nil
	}

	u, err := url.Parse(discoverURL)
	if err != nil {
		return nil, err
	}
	token := u.Path
	u.Path = ""
	cfg := client.Config{
		Transport: &http.Transport{},
		Endpoints: []string{u.String()},
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	dc := client.NewKeysAPIWithPrefix(c, "")

	ctx, cancel := context.WithTimeout(context.Background(), client.DefaultRequestTimeout)
	resp, err := dc.Get(ctx, token, nil)
	cancel()
	if err != nil {
		return nil, err
	}
	peers := make([]string, 0)
	// append non-config keys to peers
	for _, n := range resp.Node.Nodes {
		if g := path.Base(n.Key); g == "_config" || g == "_state" {
			continue
		}
		peers = append(peers, n.Value)
	}
	return peers, nil
}

func getPeersFromPeersFlag(str string, tls *TLSInfo) []string {
	peers := trimSplit(str, ",")
	for i, p := range peers {
		peers[i] = tls.Scheme() + "://" + p
	}
	return peers
}

func newDefaultClient(tls *TLSInfo) (*http.Client, error) {
	tr := &http.Transport{}
	if tls.Scheme() == "https" {
		tlsConfig, err := tls.ClientConfig()
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tlsConfig
	}
	return &http.Client{Transport: tr}, nil
}

type value struct {
	isBoolFlag bool
	s          string
}

func (v *value) String() string { return v.s }

func (v *value) Set(s string) error {
	v.s = s
	return nil
}

func (v *value) IsBoolFlag() bool { return v.isBoolFlag }

type boolFlag interface {
	flag.Value
	IsBoolFlag() bool
}

// parseConfig parses out the input config from cmdline arguments and
// environment variables.
func parseConfig(args []string) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet("full flagset", flag.ContinueOnError)
	fs.Usage = func() {}
	etcdmain.NewConfig().VisitAll(func(f *flag.Flag) {
		_, isBoolFlag := f.Value.(boolFlag)
		fs.Var(&value{isBoolFlag: isBoolFlag}, f.Name, "")
	})
	if err := fs.Parse(args); err != nil && err != flag.ErrHelp {
		return nil, err
	}
	if err := flags.SetFlagsFromEnv(fs); err != nil {
		return nil, err
	}
	return fs, nil
}

func clientTLSInfo(fs *flag.FlagSet) *TLSInfo {
	return &TLSInfo{
		CAFile:   fs.Lookup("ca-file").Value.String(),
		CertFile: fs.Lookup("cert-file").Value.String(),
		KeyFile:  fs.Lookup("key-file").Value.String(),
	}
}

func peerTLSInfo(fs *flag.FlagSet) *TLSInfo {
	return &TLSInfo{
		CAFile:   fs.Lookup("peer-ca-file").Value.String(),
		CertFile: fs.Lookup("peer-cert-file").Value.String(),
		KeyFile:  fs.Lookup("peer-key-file").Value.String(),
	}
}

func snapDir4(dataDir string) string {
	return path.Join(dataDir, "snapshot")
}

func logFile4(dataDir string) string {
	return path.Join(dataDir, "log")
}

func standbyInfo4(dataDir string) string {
	return path.Join(dataDir, "standby_info")
}

func trimSplit(s, sep string) []string {
	trimmed := strings.Split(s, sep)
	for i := range trimmed {
		trimmed[i] = strings.TrimSpace(trimmed[i])
	}
	return trimmed
}
