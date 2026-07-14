package cni

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zeeke/grout-cni/pkg/groutapi"
)

func TestGroutPortName_DeterministicAndScoped(t *testing.T) {
	assert.Equal(t, groutPortName("cid-abc", "net1"), groutPortName("cid-abc", "net1"), "must be deterministic")

	name := groutPortName("cid-abc", "net1")
	assert.LessOrEqual(t, len(name), grIfaceNameSize-1, "port name must fit IFNAMSIZ")
	assert.Regexp(t, regexp.MustCompile(`^grk-[a-z2-7]{11}$`), name)
}

func TestGroutPortName_MultiAttachDistinct(t *testing.T) {
	// Same container, different ifname => distinct grout ports (the multi-attach
	// case): grout stays a sufficient source of truth per attachment.
	assert.NotEqual(t, groutPortName("cid-abc", "net1"), groutPortName("cid-abc", "net2"))
}

func TestGroutPortName_NoCollisions(t *testing.T) {
	seen := make(map[string]string)
	for c := 0; c < 500; c++ {
		containerID := fmt.Sprintf("container-%064x", c)
		for _, ifName := range []string{"eth0", "net1", "net2", "net3"} {
			name := groutPortName(containerID, ifName)
			key := containerID + "/" + ifName
			if prev, dup := seen[name]; dup {
				t.Fatalf("port name collision %q: %s and %s", name, prev, key)
			}
			seen[name] = key
		}
	}
	assert.Len(t, seen, 2000, "all attachments got a unique port name")
}

func TestFindPortByName(t *testing.T) {
	ifaces := []groutapi.InterfaceInfo{
		{ID: 8, Type: groutapi.InterfaceTypeBridge, Name: "br-grout-net"},
		{ID: 21, Type: groutapi.InterfaceTypePort, Name: "grk-aaaaaaaaaaa", Domain: 8},
	}

	got, found := findPortByName(ifaces, "grk-aaaaaaaaaaa")
	require.True(t, found)
	assert.Equal(t, uint16(21), got.ID)
	assert.Equal(t, uint16(8), got.Domain, "bridge id comes from the port's Domain")

	_, found = findPortByName(ifaces, "grk-missing0000")
	assert.False(t, found)
}
