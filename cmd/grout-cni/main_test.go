package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zeeke/grout-cni/pkg/cni"
	"github.com/zeeke/grout-cni/pkg/groutapi"
)

func emptyIPAMDel(config *cni.PluginConf, args *skel.CmdArgs) {}
func emptyMoveLink(linkName, netnsPath string) error          { return nil }
func emptyConfigurePodIface(netnsPath, hostIfName, podIfName string, mtu int, result *types100.Result) error {
	return nil
}

func TestCmdAdd(t *testing.T) {

	ip, ipnet, err := net.ParseCIDR("10.0.0.2/24")
	require.NoError(t, err)

	savedAdd := ipamAddFunc
	savedDel := ipamDelFunc
	savedMove := moveLinkFunc
	savedConfigure := configurePodIfaceFunc

	ipamAddFunc = func(config *cni.PluginConf, args *skel.CmdArgs) (types.Result, error) {
		return &types100.Result{
			CNIVersion: types100.ImplementedSpecVersion,
			IPs: []*types100.IPConfig{{
				Address: net.IPNet{IP: ip, Mask: ipnet.Mask},
				Gateway: net.ParseIP("10.0.0.1"),
			}},
		}, nil
	}
	ipamDelFunc = emptyIPAMDel
	moveLinkFunc = emptyMoveLink
	configurePodIfaceFunc = emptyConfigurePodIface
	t.Cleanup(func() {
		ipamAddFunc = savedAdd
		ipamDelFunc = savedDel
		moveLinkFunc = savedMove
		configurePodIfaceFunc = savedConfigure
	})
	sockPath := filepath.Join(t.TempDir(), "grout.sock")
	cniConfig := `{
		"cniVersion": "1.0.0",
		"name": "grout-k-test",
		"type": "grout-k-cni",
		"groutSocketPath": "` + sockPath + `",
		"ipam": {
			"type": "host-local",
			"ranges": [[{"subnet": "10.0.0.0/24"}]]
		}
	}`

	startMockServer(t, sockPath)

	args := &skel.CmdArgs{
		ContainerID: "test-container-id",
		Netns:       "/proc/1/ns/net",
		IfName:      "eth0",
		StdinData:   []byte(cniConfig),
	}

	r, w, err := os.Pipe()
	require.NoError(t, err, "creating pipe")
	origStdout := os.Stdout
	os.Stdout = w

	err = cmdAdd(args)

	os.Stdout = origStdout
	w.Close()

	require.NoError(t, err, "cmdAdd returned error")

	var result types100.Result
	err = json.NewDecoder(r).Decode(&result)
	require.NoError(t, err, "decoding result")
	r.Close()

	require.Len(t, result.IPs, 1, "expected 1 IP config")

	resultIP := result.IPs[0]
	assert.Equal(t, "10.0.0.2", resultIP.Address.IP.String(), "IP mismatch")
	ones, _ := resultIP.Address.Mask.Size()
	assert.Equal(t, 24, ones, "expected /24 mask")
	assert.Equal(t, "10.0.0.1", resultIP.Gateway.String(), "gateway mismatch")
}

func TestCmdDel(t *testing.T) {

	savedDel := ipamDelFunc
	ipamDelFunc = emptyIPAMDel
	t.Cleanup(func() { ipamDelFunc = savedDel })
	sockPath := filepath.Join(t.TempDir(), "grout.sock")
	cniConfig := `{"cniVersion":"1.0.0","name":"test","type":"grout-k-cni","groutSocketPath":"` + sockPath + `","ipam":{"type":"host-local","ranges":[[{"subnet":"10.0.0.0/24"}]]}}`

	startMockServer(t, sockPath)

	args := &skel.CmdArgs{
		ContainerID: "test-container-id",
		Netns:       "/proc/1/ns/net",
		IfName:      "eth0",
		StdinData:   []byte(cniConfig),
	}
	require.NoError(t, cmdDel(args), "cmdDel returned error")
}

// TestCmdCheck exercises the cmdCheck wiring (config load, logging, dial, lock,
// HandleCheck) end to end against the mock grout. The mock returns an empty
// interface list, so grout is missing the port it should hold for this
// attachment — genuine drift — and CHECK must surface it as an error rather than
// the old no-op success. The port-present and pod-side paths are covered by the
// handler unit tests in pkg/cni.
func TestCmdCheck(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "grout.sock")
	cniConfig := `{"cniVersion":"1.0.0","name":"test","type":"grout-k-cni","groutSocketPath":"` + sockPath + `","ipam":{"type":"host-local","ranges":[[{"subnet":"10.0.0.0/24"}]]}}`

	startMockServer(t, sockPath)

	args := &skel.CmdArgs{
		ContainerID: "test-container-id",
		Netns:       "/proc/1/ns/net",
		IfName:      "eth0",
		StdinData:   []byte(cniConfig),
	}
	err := cmdCheck(args)
	require.Error(t, err, "CHECK must report drift when grout has lost the port")
	assert.Contains(t, err.Error(), "is missing", "error should describe the missing grout port")
}

func startMockServer(t *testing.T, sockPath string) {
	t.Helper()
	listener, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleGroutRequests(conn)
		}
	}()
	t.Cleanup(func() { listener.Close() })
}

func handleGroutRequests(conn net.Conn) {
	defer conn.Close()
	for {
		var hdr groutapi.RequestHeader
		if err := binary.Read(conn, binary.LittleEndian, &hdr); err != nil {
			return
		}
		if hdr.PayloadLen > 0 {
			payload := make([]byte, hdr.PayloadLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				return
			}
		}
		resp := groutapi.ResponseHeader{
			ForID:  hdr.ID,
			Status: 0,
		}
		if hdr.Type == groutapi.TypeIfaceAdd {
			resp.PayloadLen = 2
			_ = binary.Write(conn, binary.LittleEndian, resp)
			payload := make([]byte, 2)
			binary.LittleEndian.PutUint16(payload, 42)
			_, _ = conn.Write(payload)
		} else {
			_ = binary.Write(conn, binary.LittleEndian, resp)
		}
	}
}

func TestCmdStatus(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "grout.sock")
	startMockServer(t, sockPath)
	cniConfig := `{"cniVersion":"1.1.0","name":"test","type":"grout-k-cni","groutSocketPath":"` + sockPath + `","ipam":{"type":"host-local","ranges":[[{"subnet":"10.0.0.0/24"}]]}}`

	args := &skel.CmdArgs{StdinData: []byte(cniConfig)}
	require.NoError(t, cmdStatus(args), "STATUS should succeed when grout is reachable")
}

func TestCmdStatusNotAvailable(t *testing.T) {
	// No server listening at this path.
	sockPath := filepath.Join(t.TempDir(), "absent.sock")
	cniConfig := `{"cniVersion":"1.1.0","name":"test","type":"grout-k-cni","groutSocketPath":"` + sockPath + `","ipam":{"type":"host-local","ranges":[[{"subnet":"10.0.0.0/24"}]]}}`

	err := cmdStatus(&skel.CmdArgs{StdinData: []byte(cniConfig)})
	require.Error(t, err, "STATUS should fail when grout is unreachable")
	var cniErr *types.Error
	require.ErrorAs(t, err, &cniErr, "STATUS error must be a CNI types.Error")
	assert.Equal(t, cni.ErrPluginNotAvailable, cniErr.Code, "STATUS must report the not-available code")
}

func TestCmdGC(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "grout.sock")
	startMockServer(t, sockPath)
	cniConfig := `{"cniVersion":"1.1.0","name":"test","type":"grout-k-cni","groutSocketPath":"` + sockPath + `","ipam":{"type":"host-local","ranges":[[{"subnet":"10.0.0.0/24"}]]}}`

	// No refs stored and IPAM GC is best-effort, so GC is a clean no-op success.
	args := &skel.CmdArgs{ContainerID: "unused", StdinData: []byte(cniConfig)}
	require.NoError(t, cmdGC(args), "GC should succeed with nothing to reap")
}
