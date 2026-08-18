package backup

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

func TestRestoreNetworkFingerprintNormalizesDaemonRepresentations(t *testing.T) {
	ipv4 := network.IPAMConfig{
		Subnet:  netip.MustParsePrefix("172.30.0.0/16"),
		Gateway: netip.MustParseAddr("172.30.0.1"),
	}
	ipv6 := network.IPAMConfig{
		Subnet:     netip.MustParsePrefix("fd00:30::/64"),
		Gateway:    netip.MustParseAddr("fd00:30::1"),
		AuxAddress: map[string]netip.Addr{},
	}
	left := network.Inspect{Network: network.Network{
		Name:       "demo",
		Driver:     "bridge",
		Scope:      "local",
		EnableIPv4: false,
		EnableIPv6: true,
		IPAM: network.IPAM{
			Config: []network.IPAMConfig{ipv6, ipv4},
		},
		Options: nil,
		Labels:  map[string]string{},
	}}
	right := network.Inspect{Network: network.Network{
		Name:       "demo",
		Driver:     "bridge",
		Scope:      "local",
		EnableIPv4: true,
		EnableIPv6: true,
		IPAM: network.IPAM{
			Driver:  "default",
			Options: map[string]string{},
			Config:  []network.IPAMConfig{ipv4, ipv6},
		},
		Options: map[string]string{},
		Labels:  nil,
	}}

	if !restoreNetworkFingerprintsEqual(newRestoreNetworkFingerprint(left), newRestoreNetworkFingerprint(right)) {
		t.Fatal("semantically equivalent network fingerprints differ")
	}
}

func TestRestoreNetworkFingerprintNormalizesLegacyEnableOptions(t *testing.T) {
	legacy := network.Inspect{Network: network.Network{
		Name:       "demo",
		Driver:     "bridge",
		Scope:      "local",
		EnableIPv4: false,
		EnableIPv6: false,
		Options: map[string]string{
			restoreNetworkEnableIPv4Option: "true",
			restoreNetworkEnableIPv6Option: "false",
		},
	}}
	current := network.Inspect{Network: network.Network{
		Name:       "demo",
		Driver:     "bridge",
		Scope:      "local",
		EnableIPv4: true,
		EnableIPv6: false,
	}}
	if !restoreNetworkFingerprintsEqual(newRestoreNetworkFingerprint(legacy), newRestoreNetworkFingerprint(current)) {
		t.Fatal("legacy enable options should match current API enable fields")
	}
}

func TestRestoreNetworkCreateIPAMPreservesDaemonDriverChoice(t *testing.T) {
	input := network.IPAM{Driver: "", Options: map[string]string{}}
	createIPAM := restoreNetworkCreateIPAM(input)
	if createIPAM.Driver != "" {
		t.Fatalf("create IPAM driver = %q, want daemon-selected blank value", createIPAM.Driver)
	}
	if createIPAM.Options != nil {
		t.Fatalf("create IPAM options = %#v, want normalized nil map", createIPAM.Options)
	}
	if got := newRestoreNetworkFingerprint(network.Inspect{Network: network.Network{IPAM: input}}).IPAM.Driver; got != "default" {
		t.Fatalf("fingerprint IPAM driver = %q, want canonical default", got)
	}
}

func TestRestoredNetworkMatchesDaemonDefaultsButKeepsExplicitFieldsStrict(t *testing.T) {
	expected := network.Inspect{Network: network.Network{Name: "demo", EnableIPv4: true}}
	actual := network.Inspect{Network: network.Network{
		Name:       "demo",
		Driver:     "bridge",
		Scope:      "local",
		EnableIPv4: true,
		IPAM: network.IPAM{
			Driver: "daemon-selected-ipam",
			Config: []network.IPAMConfig{{
				Subnet:  netip.MustParsePrefix("172.31.0.0/16"),
				Gateway: netip.MustParseAddr("172.31.0.1"),
			}},
		},
		Options: map[string]string{"daemon.default": "true"},
		Labels:  map[string]string{"daemon.default": "true"},
	}}
	if !restoredNetworkMatchesCreateRequest(expected, actual) {
		t.Fatal("daemon-populated network defaults should match a blank create request")
	}

	expected.Driver = "overlay"
	if restoredNetworkMatchesCreateRequest(expected, actual) {
		t.Fatal("an explicit network driver mismatch must be rejected")
	}
	expected.Driver = ""
	expected.Internal = true
	if restoredNetworkMatchesCreateRequest(expected, actual) {
		t.Fatal("an explicit internal-network mismatch must be rejected")
	}
	expected.Internal = false
	expected.Options = map[string]string{"requested": "value"}
	actual.Options["requested"] = "value"
	if !restoredNetworkMatchesCreateRequest(expected, actual) {
		t.Fatal("daemon-added network options should not invalidate requested values")
	}
	expected.Options["requested"] = "different"
	if restoredNetworkMatchesCreateRequest(expected, actual) {
		t.Fatal("an explicit network option mismatch must be rejected")
	}
}

func TestRestoreVolumeFingerprintNormalizesDefaults(t *testing.T) {
	left := volume.Volume{Name: "data", Driver: "", Options: nil, Labels: map[string]string{}}
	right := volume.Volume{Name: "data", Driver: "local", Options: map[string]string{}, Labels: nil}
	if !restoreVolumeFingerprintsEqual(newRestoreVolumeFingerprint(left), newRestoreVolumeFingerprint(right)) {
		t.Fatal("semantically equivalent volume fingerprints differ")
	}

	actual := volume.Volume{
		Name:    "data",
		Driver:  "local",
		Options: map[string]string{"daemon.default": "true"},
		Labels:  map[string]string{"daemon.default": "true"},
	}
	if !restoredVolumeMatchesCreateRequest(left, actual) {
		t.Fatal("daemon-populated volume defaults should match a blank create request")
	}
	left.Options = map[string]string{"requested": "value"}
	actual.Options["requested"] = "value"
	if !restoredVolumeMatchesCreateRequest(left, actual) {
		t.Fatal("daemon-added volume options should not invalidate requested values")
	}
	left.Options["requested"] = "different"
	if restoredVolumeMatchesCreateRequest(left, actual) {
		t.Fatal("an explicit volume option mismatch must be rejected")
	}
}

func TestWaitForRestoredResourceRetriesNotFoundAndTransientMismatch(t *testing.T) {
	expected := network.Inspect{Network: network.Network{Name: "demo", Driver: "bridge", Internal: true}}
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := waitForRestoredResource(
		ctx,
		"network",
		"demo",
		time.Millisecond,
		func(context.Context) (network.Inspect, error) {
			attempts++
			switch attempts {
			case 1:
				return network.Inspect{}, cerrdefs.ErrNotFound
			case 2:
				return network.Inspect{Network: network.Network{Name: "demo", Driver: "bridge"}}, nil
			default:
				return expected, nil
			}
		},
		func(actual network.Inspect) bool { return restoredNetworkMatchesCreateRequest(expected, actual) },
	)
	if err != nil {
		t.Fatalf("waitForRestoredResource() error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWaitForRestoredResourceStopsOnPermanentErrorAndDeadline(t *testing.T) {
	t.Run("permanent inspect error", func(t *testing.T) {
		permanent := errors.New("permission denied")
		attempts := 0
		err := waitForRestoredResource(
			context.Background(),
			"volume",
			"data",
			time.Millisecond,
			func(context.Context) (volume.Volume, error) {
				attempts++
				return volume.Volume{}, permanent
			},
			func(volume.Volume) bool { return false },
		)
		if !errors.Is(err, permanent) || attempts != 1 {
			t.Fatalf("error = %v attempts = %d, want permanent error without retry", err, attempts)
		}
	})

	t.Run("mismatch deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		err := waitForRestoredResource(
			ctx,
			"volume",
			"data",
			time.Millisecond,
			func(context.Context) (volume.Volume, error) {
				return volume.Volume{Name: "other"}, nil
			},
			func(volume.Volume) bool { return false },
		)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline", err)
		}
	})
}
