package cni

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zeeke/grout-cni/pkg/groutapi"
)

const cniVersion = types100.ImplementedSpecVersion

type mockGroutClient struct {
	// InterfaceList
	listResult []groutapi.InterfaceInfo
	listErr    error
	listCalled bool

	// InterfaceAdd — bridge adds and member (port) adds return distinct ids.
	bridgeAddID  uint16
	bridgeAddErr error
	memberAddID  uint16
	memberAddErr error
	addCalls     []groutapi.InterfaceAddRequest

	// InterfaceDel
	interfaceDelErr    error
	interfaceDelCalled bool
	interfaceDelID     uint16
	delIDs             []uint16

	// AddressAdd
	addressAddErr     error
	addressAddCalled  bool
	addressAddIfaceID uint16
	addressAddReq     groutapi.AddressAddRequest

	// AddressFlush
	addressFlushErr error
	addressFlushIDs []uint16
}

func newMock() *mockGroutClient {
	return &mockGroutClient{bridgeAddID: 10, memberAddID: 42}
}

func (m *mockGroutClient) InterfaceList(_ groutapi.InterfaceListRequest) ([]groutapi.InterfaceInfo, error) {
	m.listCalled = true
	if m.listErr != nil {
		return nil, m.listErr
	}
	// Model deletions: a port removed via InterfaceDel no longer appears, so
	// bridge GC (which re-lists) sees the true post-delete membership.
	out := make([]groutapi.InterfaceInfo, 0, len(m.listResult))
	for _, iface := range m.listResult {
		if !m.deleted(iface.ID) {
			out = append(out, iface)
		}
	}
	return out, nil
}

func (m *mockGroutClient) InterfaceAdd(req groutapi.InterfaceAddRequest) (*groutapi.InterfaceAddResponse, error) {
	m.addCalls = append(m.addCalls, req)
	if req.Type == groutapi.InterfaceTypeBridge {
		if m.bridgeAddErr != nil {
			return nil, m.bridgeAddErr
		}
		return &groutapi.InterfaceAddResponse{IfaceID: m.bridgeAddID}, nil
	}
	if m.memberAddErr != nil {
		return nil, m.memberAddErr
	}
	return &groutapi.InterfaceAddResponse{IfaceID: m.memberAddID}, nil
}

func (m *mockGroutClient) InterfaceDel(req groutapi.InterfaceDelRequest) error {
	m.interfaceDelCalled = true
	m.interfaceDelID = req.IfaceID
	m.delIDs = append(m.delIDs, req.IfaceID)
	return m.interfaceDelErr
}

func (m *mockGroutClient) deleted(id uint16) bool {
	for _, d := range m.delIDs {
		if d == id {
			return true
		}
	}
	return false
}

func (m *mockGroutClient) AddressAdd(req groutapi.AddressAddRequest) error {
	m.addressAddCalled = true
	m.addressAddIfaceID = req.IfaceID
	m.addressAddReq = req
	return m.addressAddErr
}

func (m *mockGroutClient) AddressFlush(ifaceID uint16) error {
	m.addressFlushIDs = append(m.addressFlushIDs, ifaceID)
	return m.addressFlushErr
}

func (m *mockGroutClient) flushed(id uint16) bool {
	for _, f := range m.addressFlushIDs {
		if f == id {
			return true
		}
	}
	return false
}

// bridgeAdd returns the bridge-create request, or nil if none was made.
func (m *mockGroutClient) bridgeAdd() *groutapi.InterfaceAddRequest {
	for i := range m.addCalls {
		if m.addCalls[i].Type == groutapi.InterfaceTypeBridge {
			return &m.addCalls[i]
		}
	}
	return nil
}

// memberAdd returns the member-port request, or nil if none was made.
func (m *mockGroutClient) memberAdd() *groutapi.InterfaceAddRequest {
	for i := range m.addCalls {
		if m.addCalls[i].Type != groutapi.InterfaceTypeBridge {
			return &m.addCalls[i]
		}
	}
	return nil
}

func mockIPAMAdd(ipNet, gw string) IPAMAddFunc {
	return func(_ *PluginConf, _ *skel.CmdArgs) (types.Result, error) {
		ip, ipnet, err := net.ParseCIDR(ipNet)
		if err != nil {
			return nil, err
		}
		return &types100.Result{
			CNIVersion: cniVersion,
			IPs: []*types100.IPConfig{{
				Address: net.IPNet{IP: ip, Mask: ipnet.Mask},
				Gateway: net.ParseIP(gw),
			}},
		}, nil
	}
}

func mockIPAMAddNoIPs(_ *PluginConf, _ *skel.CmdArgs) (types.Result, error) {
	return &types100.Result{CNIVersion: cniVersion}, nil
}

func mockMoveLinkOK(_, _ string) error {
	return nil
}

func mockMoveLinkFail(_, _ string) error {
	return os.ErrInvalid
}

// mockConfigureOK records the pod interface name into the result the way the
// real implementation would, so result assertions stay meaningful.
func mockConfigureOK(_, _, _ string, _ int, result *types100.Result) error {
	if len(result.Interfaces) > 0 {
		result.Interfaces[0].Mac = "02:00:00:00:00:01"
	}
	return nil
}

func mockConfigureFail(_, _, _ string, _ int, _ *types100.Result) error {
	return os.ErrInvalid
}

func newAddConfig(t *testing.T, mock *mockGroutClient) *AddConfig {
	t.Helper()
	return &AddConfig{
		Client:            mock,
		Config:            &PluginConf{Bridge: "br-test", GroutSocketPath: filepath.Join(t.TempDir(), "grout.sock")},
		Args:              &skel.CmdArgs{ContainerID: "test-container-id-123", Netns: "/proc/1/ns/net", IfName: "net1"},
		ConfigurePodIface: mockConfigureOK,
	}
}

func newDelConfig(t *testing.T, mock *mockGroutClient) *DelConfig {
	t.Helper()
	return &DelConfig{
		Client: mock,
		Config: &PluginConf{GroutSocketPath: filepath.Join(t.TempDir(), "grout.sock")},
		Args:   &skel.CmdArgs{ContainerID: "test-container-id-123", IfName: "net1"},
	}
}

// delPortName is the deterministic grout port name for the default DEL/CHECK
// test attachment.
func delPortName() string {
	return groutPortName("test-container-id-123", "net1")
}

//
//

func TestHandleAdd_Success(t *testing.T) {
	mock := newMock() // bridge id 10, member id 42; empty list => bridge created
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")
	cfg.MoveLink = mockMoveLinkOK

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) {
		ipamDelCalled = true
	}

	result, err := HandleAdd(cfg)
	require.NoError(t, err)
	require.NotNil(t, result, "expected non-nil result")
	assert.False(t, ipamDelCalled, "IPAM del should not be called on success")

	// Bridge created, gateway assigned to it, pod port added as a member.
	br := mock.bridgeAdd()
	require.NotNil(t, br, "expected a bridge to be created")
	assert.Equal(t, "br-test", br.Name, "bridge name mismatch")

	require.True(t, mock.addressAddCalled, "expected gateway AddressAdd on the bridge")
	assert.Equal(t, uint16(10), mock.addressAddIfaceID, "gateway must be assigned to the bridge")
	assert.Equal(t, "10.0.0.1/24", mock.addressAddReq.Address.String(), "gateway address mismatch")
	assert.True(t, mock.addressAddReq.ExistOK, "bridge address add must be idempotent")

	mem := mock.memberAdd()
	require.NotNil(t, mem, "expected a member port to be added")
	assert.Equal(t, uint16(10), mem.DomainID, "member must reference the bridge via DomainID")
	assert.Equal(t, groutPortName("test-container-id-123", "net1"), mem.Name, "member port name mismatch")

	cniResult, ok := result.(*types100.Result)
	require.True(t, ok, "expected *types100.Result")
	require.Len(t, cniResult.IPs, 1, "expected 1 IP")
	assert.Equal(t, "10.0.0.5/24", cniResult.IPs[0].Address.String(), "IP mismatch")

	// Result describes the pod interface and binds the IP to it.
	require.Len(t, cniResult.Interfaces, 1, "expected 1 interface in result")
	assert.Equal(t, "net1", cniResult.Interfaces[0].Name, "interface name mismatch")
	assert.Equal(t, "/proc/1/ns/net", cniResult.Interfaces[0].Sandbox, "interface sandbox mismatch")
	assert.Equal(t, "02:00:00:00:00:01", cniResult.Interfaces[0].Mac, "interface MAC not set by configure")
	require.NotNil(t, cniResult.IPs[0].Interface, "IP must reference an interface")
	assert.Equal(t, 0, *cniResult.IPs[0].Interface, "IP should reference interface index 0")
}

func TestHandleAdd_Virtio(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.Config.InterfaceType = InterfaceTypeVirtio
	cfg.Config.GroutSocketPath = filepath.Join(t.TempDir(), "grout.sock")
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")

	moveCalled, configCalled := false, false
	cfg.MoveLink = func(_, _ string) error { moveCalled = true; return nil }
	cfg.ConfigurePodIface = func(_, _, _ string, _ int, _ *types100.Result) error { configCalled = true; return nil }

	result, err := HandleAdd(cfg)
	require.NoError(t, err)

	assert.False(t, moveCalled, "virtio must not move a kernel link")
	assert.False(t, configCalled, "virtio must not configure a netns interface")

	mem := mock.memberAdd()
	require.NotNil(t, mem, "a grout port should be added")
	assert.NotEmpty(t, mem.VhostUserPath, "virtio must request a vhost-user port")
	assert.Equal(t, uint16(10), mem.DomainID, "the vhost port joins the bridge")

	res, ok := result.(*types100.Result)
	require.True(t, ok)
	require.Len(t, res.Interfaces, 1)
	assert.NotEmpty(t, res.Interfaces[0].SocketPath, "result must report the vhost socket path")
	assert.Empty(t, res.Interfaces[0].Sandbox, "virtio interface has no netns sandbox")
	require.Len(t, res.IPs, 1)
	assert.Equal(t, "10.0.0.5/24", res.IPs[0].Address.String())

	// The reported vhost socket path is deterministic from the port name, so DEL
	// can reconstruct and remove it without a stored ref.
	wantSock := vhostSocketPath(cfg.Config.GroutSocketPath, groutPortName("test-container-id-123", "net1"))
	assert.Equal(t, wantSock, res.Interfaces[0].SocketPath, "socket path must be the deterministic one")
}

func TestHandleAdd_ExistingBridge(t *testing.T) {
	mock := newMock()
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 7, Type: groutapi.InterfaceTypeBridge, Name: "br-test"},
	}
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")
	cfg.MoveLink = mockMoveLinkOK

	_, err := HandleAdd(cfg)
	require.NoError(t, err)

	assert.Nil(t, mock.bridgeAdd(), "existing bridge must not be re-created")
	assert.Equal(t, uint16(7), mock.addressAddIfaceID, "gateway must go on the existing bridge")
	mem := mock.memberAdd()
	require.NotNil(t, mem)
	assert.Equal(t, uint16(7), mem.DomainID, "member must reference the existing bridge id")
}

func TestHandleAdd_BridgeListFail(t *testing.T) {
	mock := newMock()
	mock.listErr = os.ErrInvalid
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	_, err := HandleAdd(cfg)
	require.Error(t, err)
	assert.Empty(t, mock.addCalls, "no interface should be added if listing fails")
	assert.True(t, ipamDelCalled, "IPAM del should be called on bridge list failure")
}

func TestHandleAdd_BridgeCreateFail(t *testing.T) {
	mock := newMock()
	mock.bridgeAddErr = os.ErrInvalid
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	_, err := HandleAdd(cfg)
	require.Error(t, err)
	assert.Nil(t, mock.memberAdd(), "member must not be added if bridge create fails")
	assert.False(t, mock.addressAddCalled, "no gateway address if bridge create fails")
	assert.True(t, ipamDelCalled, "IPAM del should be called on bridge create failure")
}

func TestHandleAdd_BridgeAddressFail(t *testing.T) {
	mock := newMock()
	mock.addressAddErr = os.ErrInvalid
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	_, err := HandleAdd(cfg)
	require.Error(t, err)

	require.NotNil(t, mock.bridgeAdd(), "bridge is created before the gateway address")
	assert.Nil(t, mock.memberAdd(), "member must not be added if the bridge address fails")
	assert.False(t, mock.interfaceDelCalled, "the shared bridge must not be torn down")
	assert.True(t, ipamDelCalled, "IPAM del should be called on bridge address failure")
}

func TestHandleAdd_MemberAddFail(t *testing.T) {
	mock := newMock()
	mock.memberAddErr = os.ErrInvalid
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")
	cfg.MoveLink = mockMoveLinkOK

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	_, err := HandleAdd(cfg)
	require.Error(t, err)

	assert.True(t, mock.addressAddCalled, "gateway is assigned before the member add")
	assert.False(t, mock.interfaceDelCalled, "no member exists to roll back")
	assert.True(t, ipamDelCalled, "IPAM del should be called on member add failure")
}

func TestHandleAdd_MoveLinkFail(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")
	cfg.MoveLink = mockMoveLinkFail

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	_, err := HandleAdd(cfg)
	require.Error(t, err)

	assert.True(t, mock.interfaceDelCalled, "member port should be rolled back")
	assert.Equal(t, uint16(42), mock.interfaceDelID, "rollback must delete the member port")
	assert.True(t, ipamDelCalled, "IPAM del should be called on rollback")
}

func TestHandleAdd_ConfigureFail(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")
	cfg.MoveLink = mockMoveLinkOK
	cfg.ConfigurePodIface = mockConfigureFail

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	_, err := HandleAdd(cfg)
	require.Error(t, err)

	assert.True(t, mock.interfaceDelCalled, "member port should be rolled back")
	assert.Equal(t, uint16(42), mock.interfaceDelID, "rollback must delete the member port")
	assert.True(t, ipamDelCalled, "IPAM del should be called on rollback")
}

func TestHandleAdd_NoNetns(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.Args.Netns = ""
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	result, err := HandleAdd(cfg)
	require.NoError(t, err)
	require.NotNil(t, result, "expected non-nil result")

	require.NotNil(t, mock.memberAdd(), "member port should be added")
	assert.True(t, mock.addressAddCalled, "gateway should be assigned to the bridge")
	assert.False(t, ipamDelCalled, "IPAM del should not be called on success")
}

func TestHandleAdd_IPv6(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAdd("fd00::5/64", "fd00::1")
	cfg.MoveLink = mockMoveLinkOK

	result, err := HandleAdd(cfg)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.True(t, mock.addressAddCalled, "expected gateway AddressAdd on the bridge")
	assert.Equal(t, "fd00::1/64", mock.addressAddReq.Address.String(), "IPv6 gateway mismatch")
}

func TestHandleAdd_DualStack(t *testing.T) {
	mock := newMock()
	mock.addressAddErr = nil
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = func(_ *PluginConf, _ *skel.CmdArgs) (types.Result, error) {
		return &types100.Result{
			CNIVersion: cniVersion,
			IPs: []*types100.IPConfig{
				{
					Address: net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
					Gateway: net.ParseIP("10.0.0.1"),
				},
				{
					Address: net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)},
					Gateway: net.ParseIP("fd00::1"),
				},
			},
		}, nil
	}
	cfg.MoveLink = mockMoveLinkOK

	var addCalls []groutapi.AddressAddRequest
	cfg.Client = &dualStackMock{mockGroutClient: mock, addrAddCalls: &addCalls}

	result, err := HandleAdd(cfg)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, addCalls, 2, "expected two gateway AddressAdd calls (IPv4 + IPv6)")
	assert.Equal(t, "10.0.0.1/24", addCalls[0].Address.String())
	assert.Equal(t, "fd00::1/64", addCalls[1].Address.String())

	cniResult, ok := result.(*types100.Result)
	require.True(t, ok)
	require.Len(t, cniResult.IPs, 2, "dual-stack result should have 2 IPs")
}

// dualStackMock wraps mockGroutClient to track multiple AddressAdd calls.
type dualStackMock struct {
	*mockGroutClient
	addrAddCalls *[]groutapi.AddressAddRequest
}

func (m *dualStackMock) AddressAdd(req groutapi.AddressAddRequest) error {
	*m.addrAddCalls = append(*m.addrAddCalls, req)
	return m.mockGroutClient.AddressAdd(req)
}

func TestHandleAdd_NoIPs(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = mockIPAMAddNoIPs
	cfg.MoveLink = mockMoveLinkOK

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	result, err := HandleAdd(cfg)
	require.NoError(t, err)
	require.NotNil(t, result, "expected non-nil result")

	require.NotNil(t, mock.memberAdd(), "member port should be added")
	assert.False(t, mock.addressAddCalled, "no gateway address when IPAM returns no IPs")
	assert.False(t, ipamDelCalled, "IPAM del should not be called when no IPs but no error")
}

func TestGatewaysFor_IPv4Only(t *testing.T) {
	result := &types100.Result{
		IPs: []*types100.IPConfig{{
			Address: net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
			Gateway: net.ParseIP("10.0.0.1"),
		}},
	}
	gws := gatewaysFor(result)
	require.Len(t, gws, 1)
	assert.Equal(t, "10.0.0.1/24", gws[0].String())
}

func TestGatewaysFor_IPv6Only(t *testing.T) {
	result := &types100.Result{
		IPs: []*types100.IPConfig{{
			Address: net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)},
			Gateway: net.ParseIP("fd00::1"),
		}},
	}
	gws := gatewaysFor(result)
	require.Len(t, gws, 1)
	assert.Equal(t, "fd00::1/64", gws[0].String())
}

func TestGatewaysFor_DualStack(t *testing.T) {
	result := &types100.Result{
		IPs: []*types100.IPConfig{
			{
				Address: net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
				Gateway: net.ParseIP("10.0.0.1"),
			},
			{
				Address: net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)},
				Gateway: net.ParseIP("fd00::1"),
			},
		},
	}
	gws := gatewaysFor(result)
	require.Len(t, gws, 2)
	assert.Equal(t, "10.0.0.1/24", gws[0].String())
	assert.Equal(t, "fd00::1/64", gws[1].String())
}

func TestGatewaysFor_DeriveGateway(t *testing.T) {
	result := &types100.Result{
		IPs: []*types100.IPConfig{{
			Address: net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)},
		}},
	}
	gws := gatewaysFor(result)
	require.Len(t, gws, 1)
	assert.Equal(t, "fd00::1", gws[0].IP.String(), "should derive first host in IPv6 subnet")
}

func TestGatewaysFor_Empty(t *testing.T) {
	result := &types100.Result{}
	gws := gatewaysFor(result)
	assert.Empty(t, gws)
}

func TestPodMACFromResult_IPv6Only(t *testing.T) {
	result := &types100.Result{
		IPs: []*types100.IPConfig{{
			Address: net.IPNet{IP: net.ParseIP("fd00::abcd"), Mask: net.CIDRMask(64, 128)},
		}},
	}
	mac := podMACFromResult(result)
	require.NotNil(t, mac, "should derive MAC from IPv6")
	assert.Equal(t, byte(0x02), mac[0], "must be locally-administered")
	assert.Equal(t, byte(0x00), mac[1])
	ip6 := net.ParseIP("fd00::abcd").To16()
	assert.Equal(t, ip6[12], mac[2])
	assert.Equal(t, ip6[13], mac[3])
	assert.Equal(t, ip6[14], mac[4])
	assert.Equal(t, ip6[15], mac[5])
}

func TestPodMACFromResult_DualStackPrefersIPv4(t *testing.T) {
	result := &types100.Result{
		IPs: []*types100.IPConfig{
			{Address: net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)}},
			{Address: net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)}},
		},
	}
	mac := podMACFromResult(result)
	require.NotNil(t, mac)
	assert.Equal(t, net.HardwareAddr{0x02, 0x00, 10, 0, 0, 5}, mac, "dual-stack should use IPv4 for MAC")
}

func TestHandleAdd_IPAMAddFail(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.IPAMAdd = func(_ *PluginConf, _ *skel.CmdArgs) (types.Result, error) {
		return nil, os.ErrInvalid
	}

	_, err := HandleAdd(cfg)
	require.Error(t, err)

	assert.False(t, mock.listCalled, "grout should not be touched if IPAM fails")
	assert.Empty(t, mock.addCalls, "no interface should be added if IPAM fails")
}

//
//

func TestHandleDel_Success(t *testing.T) {
	mock := newMock()
	// grout's view: the pod port (named by the hash) with no bridge/domain.
	mock.listResult = []groutapi.InterfaceInfo{{ID: 42, Type: groutapi.InterfaceTypePort, Name: delPortName()}}
	cfg := newDelConfig(t, mock)

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	require.NoError(t, HandleDel(cfg))

	assert.True(t, mock.interfaceDelCalled, "expected InterfaceDel to be called")
	assert.Equal(t, uint16(42), mock.interfaceDelID, "expected InterfaceDel ifaceID 42")
	assert.True(t, ipamDelCalled, "IPAM del should be called")
}

func TestHandleDel_NoPort(t *testing.T) {
	mock := &mockGroutClient{} // empty list, no legacy ref
	cfg := newDelConfig(t, mock)

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	require.NoError(t, HandleDel(cfg))

	assert.False(t, mock.interfaceDelCalled, "InterfaceDel should not be called when the port is absent")
	assert.True(t, ipamDelCalled, "IPAM del should be called even when the port is absent")
}

func TestHandleDel_InterfaceDelError(t *testing.T) {
	mock := &mockGroutClient{interfaceDelErr: os.ErrInvalid}
	mock.listResult = []groutapi.InterfaceInfo{{ID: 42, Type: groutapi.InterfaceTypePort, Name: delPortName()}}
	cfg := newDelConfig(t, mock)

	ipamDelCalled := false
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) { ipamDelCalled = true }

	require.NoError(t, HandleDel(cfg))

	assert.True(t, mock.interfaceDelCalled, "expected InterfaceDel to be called")
	assert.True(t, ipamDelCalled, "IPAM del should be called even if InterfaceDel fails")
}

func TestHandleDel_GCEmptyBridge(t *testing.T) {
	mock := newMock()
	// The pod port is a member of bridge 10; after its removal the bridge empties.
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 10, Type: groutapi.InterfaceTypeBridge, Name: "br-test"},
		{ID: 42, Type: groutapi.InterfaceTypePort, Name: delPortName(), Domain: 10},
	}
	cfg := newDelConfig(t, mock)
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) {}

	require.NoError(t, HandleDel(cfg))
	assert.True(t, mock.deleted(42), "member port should be deleted")
	assert.True(t, mock.deleted(10), "empty bridge should be garbage-collected")
	assert.True(t, mock.flushed(10), "bridge addresses must be flushed before deleting it")
}

func TestHandleDel_KeepBridgeWithMembers(t *testing.T) {
	mock := newMock()
	// Another port still references bridge 10 after the pod port is removed.
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 10, Type: groutapi.InterfaceTypeBridge, Name: "br-test"},
		{ID: 42, Type: groutapi.InterfaceTypePort, Name: delPortName(), Domain: 10},
		{ID: 43, Type: groutapi.InterfaceTypePort, Name: "grk-otherport0", Domain: 10},
	}
	cfg := newDelConfig(t, mock)
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) {}

	require.NoError(t, HandleDel(cfg))
	assert.True(t, mock.deleted(42), "member port should be deleted")
	assert.False(t, mock.deleted(10), "bridge with remaining members must not be deleted")
}

func TestHandleDel_GCSkipsRecycledNonBridge(t *testing.T) {
	mock := newMock()
	// The port's domain id now belongs to a non-bridge interface (id reuse).
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 10, Type: groutapi.InterfaceTypePort, Name: "grout-y"},
		{ID: 42, Type: groutapi.InterfaceTypePort, Name: delPortName(), Domain: 10},
	}
	cfg := newDelConfig(t, mock)
	cfg.IPAMDel = func(_ *PluginConf, _ *skel.CmdArgs) {}

	require.NoError(t, HandleDel(cfg))
	assert.True(t, mock.deleted(42), "member port should be deleted")
	assert.False(t, mock.deleted(10), "must not delete an id that is no longer a bridge")
}

// portPresentMock returns a mock whose interface list contains just the pod port
// for the default DEL/CHECK test attachment.
func portPresentMock() *mockGroutClient {
	mock := newMock()
	mock.listResult = []groutapi.InterfaceInfo{{ID: 42, Type: groutapi.InterfaceTypePort, Name: delPortName()}}
	return mock
}

// TestHandleCheck_Present: grout port present and no netns to inspect (grout-side
// check only) => CHECK passes.
func TestHandleCheck_Present(t *testing.T) {
	cfg := newDelConfig(t, portPresentMock())
	assert.NoError(t, HandleCheck(cfg), "CHECK should pass when the port exists")
}

// TestHandleCheck_Absent: a missing grout port is drift, so CHECK must error.
func TestHandleCheck_Absent(t *testing.T) {
	mock := newMock() // empty list => port missing
	cfg := newDelConfig(t, mock)
	assert.Error(t, HandleCheck(cfg), "CHECK must report drift when the grout port is absent")
}

// TestHandleCheck_ListError: a failure to list interfaces surfaces as an error.
func TestHandleCheck_ListError(t *testing.T) {
	mock := newMock()
	mock.listErr = os.ErrInvalid
	cfg := newDelConfig(t, mock)
	assert.Error(t, HandleCheck(cfg), "CHECK must surface an interface-list failure")
}

// TestHandleCheck_TAPPodLinkOK: port present, pod netns set, and the injected
// pod-side verification succeeds => CHECK passes and the TAP path is exercised.
func TestHandleCheck_TAPPodLinkOK(t *testing.T) {
	cfg := newDelConfig(t, portPresentMock())
	cfg.Args.Netns = "/proc/1/ns/net"
	verified := false
	cfg.VerifyPodIface = func(netnsPath, podIfName string, _ []*net.IPNet) error {
		verified = true
		assert.Equal(t, "/proc/1/ns/net", netnsPath)
		assert.Equal(t, "net1", podIfName)
		return nil
	}
	require.NoError(t, HandleCheck(cfg))
	assert.True(t, verified, "TAP CHECK should verify the pod-side interface")
}

// TestHandleCheck_TAPPodLinkBad: port present but the pod-side interface is
// missing/down (injected verify fails) => CHECK errors.
func TestHandleCheck_TAPPodLinkBad(t *testing.T) {
	cfg := newDelConfig(t, portPresentMock())
	cfg.Args.Netns = "/proc/1/ns/net"
	cfg.VerifyPodIface = func(_, _ string, _ []*net.IPNet) error {
		return fmt.Errorf("link is down")
	}
	err := HandleCheck(cfg)
	require.Error(t, err, "CHECK must error when the pod interface is missing or down")
	assert.Contains(t, err.Error(), "verifying pod interface")
}

// TestHandleCheck_VirtioSocketPresent: virtio port present and its vhost-user
// socket file exists => CHECK passes without touching the pod netns.
func TestHandleCheck_VirtioSocketPresent(t *testing.T) {
	cfg := newDelConfig(t, portPresentMock())
	cfg.Config.InterfaceType = InterfaceTypeVirtio
	cfg.Args.Netns = "/proc/1/ns/net"
	cfg.VerifyPodIface = func(_, _ string, _ []*net.IPNet) error {
		t.Fatal("virtio CHECK must not touch the pod netns")
		return nil
	}
	sock := vhostSocketPath(cfg.Config.GroutSocketPath, delPortName())
	require.NoError(t, os.MkdirAll(filepath.Dir(sock), 0o755))
	require.NoError(t, os.WriteFile(sock, []byte{}, 0o600))

	assert.NoError(t, HandleCheck(cfg), "virtio CHECK should pass when the vhost socket exists")
}

// TestHandleCheck_VirtioSocketMissing: virtio port present but its vhost-user
// socket is gone => CHECK errors.
func TestHandleCheck_VirtioSocketMissing(t *testing.T) {
	cfg := newDelConfig(t, portPresentMock())
	cfg.Config.InterfaceType = InterfaceTypeVirtio
	err := HandleCheck(cfg)
	require.Error(t, err, "virtio CHECK must fail when the vhost socket is missing")
	assert.Contains(t, err.Error(), "vhost-user socket")
}

// TestHandleCheck_ParsesPrevResultIPs: with a prevResult on the config, the TAP
// path parses the expected pod addresses and passes them to the verifier.
func TestHandleCheck_ParsesPrevResultIPs(t *testing.T) {
	cfg := newDelConfig(t, portPresentMock())
	cfg.Args.Netns = "/proc/1/ns/net"
	cfg.Config.CNIVersion = cniVersion
	cfg.Config.RawPrevResult = map[string]interface{}{
		"cniVersion": cniVersion,
		"interfaces": []interface{}{map[string]interface{}{"name": "net1", "sandbox": "/proc/1/ns/net"}},
		"ips": []interface{}{
			map[string]interface{}{"address": "10.0.0.5/24", "interface": float64(0)},
			map[string]interface{}{"address": "fd00::5/64", "interface": float64(0)},
		},
	}

	var got []*net.IPNet
	cfg.VerifyPodIface = func(_, _ string, expectedIPs []*net.IPNet) error {
		got = expectedIPs
		return nil
	}
	require.NoError(t, HandleCheck(cfg))
	require.Len(t, got, 2, "both prevResult addresses should be extracted")
	assert.Equal(t, "10.0.0.5/24", got[0].String())
	assert.Equal(t, "fd00::5/64", got[1].String())
}

func TestExpectedPodIPs_NoPrevResult(t *testing.T) {
	ips, err := expectedPodIPs(&PluginConf{})
	require.NoError(t, err)
	assert.Nil(t, ips, "no prevResult => no expected addresses")
}

func TestHandleAdd_MTU(t *testing.T) {
	mock := newMock()
	cfg := newAddConfig(t, mock)
	cfg.Config.MTU = 1400
	cfg.IPAMAdd = mockIPAMAdd("10.0.0.5/24", "10.0.0.1")
	cfg.MoveLink = mockMoveLinkOK

	var gotMTU int
	cfg.ConfigurePodIface = func(_, _, _ string, mtu int, r *types100.Result) error {
		gotMTU = mtu
		if len(r.Interfaces) > 0 {
			r.Interfaces[0].Mac = "02:00:00:00:00:01"
		}
		return nil
	}

	_, err := HandleAdd(cfg)
	require.NoError(t, err)

	mem := mock.memberAdd()
	require.NotNil(t, mem)
	assert.Equal(t, uint16(1400), mem.MTU, "member port MTU should be set from config")
	assert.Equal(t, 1400, gotMTU, "pod interface MTU should be passed to configure")
}

//
//

func TestLoadConfig(t *testing.T) {
	raw := `{
		"cniVersion": "1.0.0",
		"name": "grout-k-test",
		"type": "grout-cni",
		"groutSocketPath": "/tmp/test-grout.sock",
		"ipam": {
			"type": "host-local",
			"ranges": [ [{"subnet": "10.0.0.0/24"}] ]
		}
	}`

	conf, err := LoadConfig([]byte(raw))
	require.NoError(t, err, "LoadConfig failed")
	assert.Equal(t, "grout-k-test", conf.Name)
	assert.Equal(t, "/tmp/test-grout.sock", conf.GroutSocketPath)
	require.NotNil(t, conf.IPAM)
	assert.Equal(t, "host-local", conf.IPAM.Type)
}

func TestLoadConfigDefaults(t *testing.T) {
	raw := `{
		"cniVersion": "1.0.0",
		"name": "grout-k-test",
		"type": "grout-cni",
		"ipam": {
			"type": "host-local",
			"ranges": [ [{"subnet": "10.0.0.0/24"}] ]
		}
	}`

	conf, err := LoadConfig([]byte(raw))
	require.NoError(t, err, "LoadConfig failed")
	assert.Equal(t, DefaultSocketPath, conf.GroutSocketPath)
	assert.Equal(t, "br-grout-k-test", conf.Bridge, "bridge should default from the network name")
}

func TestLoadConfigMTUBounds(t *testing.T) {
	base := `{"cniVersion":"1.0.0","name":"n","type":"grout-cni","ipam":{"type":"host-local"},"mtu":%d}`
	for _, mtu := range []int{-1, 70000} {
		_, err := LoadConfig([]byte(fmt.Sprintf(base, mtu)))
		require.Error(t, err, "mtu %d should be rejected", mtu)
	}
	conf, err := LoadConfig([]byte(fmt.Sprintf(base, 1400)))
	require.NoError(t, err)
	assert.Equal(t, 1400, conf.MTU)
}

func TestLoadConfigBridgeNameTooLong(t *testing.T) {
	raw := `{"cniVersion":"1.0.0","name":"n","type":"grout-cni","bridge":"this-name-is-way-too-long","ipam":{"type":"host-local"}}`
	_, err := LoadConfig([]byte(raw))
	require.Error(t, err, "an over-long explicit bridge name should be rejected")
}

func TestBridgeNameForLongNetwork(t *testing.T) {
	// Distinct long network names must not collapse to the same bridge name.
	a := bridgeNameFor("a-very-long-network-name-one")
	b := bridgeNameFor("a-very-long-network-name-two")
	assert.LessOrEqual(t, len(a), grIfaceNameSize-1)
	assert.LessOrEqual(t, len(b), grIfaceNameSize-1)
	assert.NotEqual(t, a, b, "distinct networks must yield distinct bridge names")
}

func TestLoadConfigInterfaceType(t *testing.T) {
	base := `{"cniVersion":"1.0.0","name":"n","type":"grout-cni","ipam":{"type":"host-local"}%s}`

	conf, err := LoadConfig([]byte(fmt.Sprintf(base, "")))
	require.NoError(t, err)
	assert.Equal(t, InterfaceTypeTAP, conf.InterfaceType, "default interface type should be tap")

	conf, err = LoadConfig([]byte(fmt.Sprintf(base, `,"interfaceType":"virtio"`)))
	require.NoError(t, err)
	assert.Equal(t, InterfaceTypeVirtio, conf.InterfaceType)

	_, err = LoadConfig([]byte(fmt.Sprintf(base, `,"interfaceType":"bogus"`)))
	require.Error(t, err, "an unknown interface type should be rejected")
}

func TestLoadConfigLogLevel(t *testing.T) {
	base := `{"cniVersion":"1.0.0","name":"n","type":"grout-cni","ipam":{"type":"host-local"}%s}`

	conf, err := LoadConfig([]byte(fmt.Sprintf(base, "")))
	require.NoError(t, err)
	assert.Empty(t, conf.LogLevel, "logLevel is optional and defaults to unset")

	conf, err = LoadConfig([]byte(fmt.Sprintf(base, `,"logLevel":"debug"`)))
	require.NoError(t, err)
	assert.Equal(t, "debug", conf.LogLevel)

	_, err = LoadConfig([]byte(fmt.Sprintf(base, `,"logLevel":"verbose"`)))
	require.Error(t, err, "an unknown log level should be rejected")
}

func TestLoadConfigBridgeOverride(t *testing.T) {
	raw := `{
		"cniVersion": "1.0.0",
		"name": "grout-k-test",
		"type": "grout-cni",
		"bridge": "br-custom",
		"ipam": { "type": "host-local", "ranges": [ [{"subnet": "10.0.0.0/24"}] ] }
	}`

	conf, err := LoadConfig([]byte(raw))
	require.NoError(t, err, "LoadConfig failed")
	assert.Equal(t, "br-custom", conf.Bridge, "explicit bridge name should be preserved")
}

func TestLoadConfigMissingIPAM(t *testing.T) {
	raw := `{
		"cniVersion": "1.0.0",
		"name": "grout-k-test",
		"type": "grout-cni"
	}`

	_, err := LoadConfig([]byte(raw))
	require.Error(t, err, "expected error for missing IPAM config")
}

func TestLoadConfigMissingName(t *testing.T) {
	raw := `{
		"cniVersion": "1.0.0",
		"type": "grout-cni",
		"ipam": {
			"type": "host-local"
		}
	}`

	_, err := LoadConfig([]byte(raw))
	require.Error(t, err, "expected error for missing name")
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	raw := `{invalid json}`

	_, err := LoadConfig([]byte(raw))
	require.Error(t, err, "expected error for invalid JSON")
}

func TestFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	lock1, err := NewFileLock(path)
	require.NoError(t, err, "NewFileLock")
	defer func() { _ = lock1.Close() }()

	require.NoError(t, lock1.Lock(), "Lock")
	require.NoError(t, lock1.Unlock(), "Unlock")
}

func TestNewFileLockCreatesParentDir(t *testing.T) {
	// The node may not have the data dir yet; NewFileLock must create it.
	path := filepath.Join(t.TempDir(), "grout-k-cni", "cni.lock")
	lock, err := NewFileLock(path)
	require.NoError(t, err, "NewFileLock should create the missing parent dir")
	defer func() { _ = lock.Close() }()
	assert.True(t, fileExists(path), "lock file should exist after NewFileLock")
}

func TestLinkExists(t *testing.T) {
	assert.False(t, linkExists("nonexistent-link-xyz"), "expected false for non-existent link")
}

func TestFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test-file")
	assert.False(t, fileExists(path), "expected false for non-existent file")

	err := os.WriteFile(path, []byte("test"), 0o644)
	require.NoError(t, err, "WriteFile")

	assert.True(t, fileExists(path), "expected true for existing file")
}

// TestEnsureBridge_ReusesExistingOnCreateConflict covers the case observed in
// the mixed TAP/virtio e2e lane: the interface list does not surface the bridge
// under its expected bridge type, so the strict find misses it and the create
// conflicts (grout EEXIST). ensureBridge must then re-find it by name (grout
// names are globally unique) and reuse it rather than failing.
func TestEnsureBridge_ReusesExistingOnCreateConflict(t *testing.T) {
	m := newMock()
	// Bridge present in the list, but reported with a non-bridge type so the
	// strict (name+type) find skips it.
	m.listResult = []groutapi.InterfaceInfo{
		{ID: 7, Name: "br-shared", Type: groutapi.InterfaceTypePort},
	}
	m.bridgeAddErr = fmt.Errorf("grout error: status 17")

	id, err := ensureBridge(m, "br-shared")
	require.NoError(t, err, "should reuse the existing bridge instead of failing")
	assert.Equal(t, uint16(7), id, "should return the existing interface's id, found by name")
}

// TestEnsureBridge_CreateConflictNotFoundReportsInterfaces verifies that when a
// create conflict cannot be reconciled (the bridge is genuinely absent from the
// list the CNI received), the error embeds what the list did return so the miss
// is diagnosable from the CNI error alone.
func TestEnsureBridge_CreateConflictNotFoundReportsInterfaces(t *testing.T) {
	m := newMock()
	m.listResult = []groutapi.InterfaceInfo{
		{ID: 2, Name: "br-other", Type: groutapi.InterfaceTypeBridge},
	}
	m.bridgeAddErr = fmt.Errorf("grout error: status 17")

	_, err := ensureBridge(m, "br-shared")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "interfaces seen:", "error should enumerate the interfaces the list returned")
	assert.Contains(t, err.Error(), "br-other(id=2", "error should include the interfaces seen for diagnosis")
}

// TestRefStorePerIfnameIsolation is the multi-attachment regression: two
// attachments sharing a container id but with different ifnames must not
// clobber each other, and deleting one must leave the other intact.
func newGCConfig(t *testing.T, mock *mockGroutClient) *GCConfig {
	t.Helper()
	return &GCConfig{
		Client: mock,
		Config: &PluginConf{
			NetConf:         types.NetConf{Name: "grout-net"},
			Bridge:          "br-grout-net",
			GroutSocketPath: filepath.Join(t.TempDir(), "grout.sock"),
		},
		Args:   &skel.CmdArgs{ContainerID: "unused"},
		IPAMGC: func(*PluginConf, *skel.CmdArgs) error { return nil },
	}
}

// TestHandleGC_ReapsStalePort reaps a grout-k port on this network's bridge that
// the runtime did not list as valid, GC-ing the bridge once it empties.
func TestHandleGC_ReapsStalePort(t *testing.T) {
	mock := newMock()
	keep := groutPortName("keep", "net1")
	stale := groutPortName("stale", "net1")
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 10, Type: groutapi.InterfaceTypeBridge, Name: "br-grout-net"},
		{ID: 42, Type: groutapi.InterfaceTypePort, Name: keep, Domain: 10},
		{ID: 43, Type: groutapi.InterfaceTypePort, Name: stale, Domain: 10},
	}
	cfg := newGCConfig(t, mock)
	// Runtime says only (keep, net1) is still valid; (stale, net1) is not.
	cfg.Config.ValidAttachments = []types.GCAttachment{{ContainerID: "keep", IfName: "net1"}}

	require.NoError(t, HandleGC(cfg))

	assert.True(t, mock.deleted(43), "stale port must be reaped")
	assert.False(t, mock.deleted(42), "valid port must not be reaped")
	assert.False(t, mock.deleted(10), "bridge still has the valid member")
}

// TestHandleGC_ReapEmptiesBridge: reaping the last member GCs the bridge.
func TestHandleGC_ReapEmptiesBridge(t *testing.T) {
	mock := newMock()
	stale := groutPortName("stale", "net1")
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 10, Type: groutapi.InterfaceTypeBridge, Name: "br-grout-net"},
		{ID: 43, Type: groutapi.InterfaceTypePort, Name: stale, Domain: 10},
	}
	cfg := newGCConfig(t, mock) // no valid attachments => stale is reaped

	require.NoError(t, HandleGC(cfg))

	assert.True(t, mock.deleted(43), "stale port must be reaped")
	assert.True(t, mock.deleted(10), "now-empty bridge should be garbage-collected")
	assert.True(t, mock.flushed(10), "bridge addresses must be flushed before deleting it")
}

// TestHandleGC_ScopedByBridge ensures GC never reaps another network's ports or
// non-grout-k interfaces, since the reap is scoped to this network's bridge.
func TestHandleGC_ScopedByBridge(t *testing.T) {
	mock := newMock()
	other := groutPortName("other", "net1")
	mock.listResult = []groutapi.InterfaceInfo{
		{ID: 10, Type: groutapi.InterfaceTypeBridge, Name: "br-grout-net"},
		{ID: 20, Type: groutapi.InterfaceTypeBridge, Name: "br-grout-virtio"},
		{ID: 99, Type: groutapi.InterfaceTypePort, Name: other, Domain: 20},  // different bridge
		{ID: 77, Type: groutapi.InterfaceTypePort, Name: "eth0", Domain: 10}, // not grout-k
	}
	cfg := newGCConfig(t, mock) // GC network = grout-net (bridge 10), no valid attachments

	require.NoError(t, HandleGC(cfg))

	assert.False(t, mock.deleted(99), "another network's port (different bridge) must not be reaped")
	assert.False(t, mock.deleted(77), "a non-grout-k interface must not be reaped")
}

// TestHandleGC_DelegatesIPAM confirms GC delegates to the IPAM plugin's GC.
func TestHandleGC_DelegatesIPAM(t *testing.T) {
	mock := newMock()
	cfg := newGCConfig(t, mock)
	ipamGCCalled := false
	cfg.IPAMGC = func(*PluginConf, *skel.CmdArgs) error {
		ipamGCCalled = true
		return nil
	}
	require.NoError(t, HandleGC(cfg))
	assert.True(t, ipamGCCalled, "GC must delegate to the IPAM plugin")
}
