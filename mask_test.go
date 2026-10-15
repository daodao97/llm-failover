package failover

import "testing"

func TestChannelLabelAppendsPlainGroupID(t *testing.T) {
	if got, want := ChannelLabel(&Channel{Id: 78, ChannelGroupID: 10}), MaskChannelName("78")+" 10"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	if got, want := ChannelLabel(&Channel{Id: 78}), MaskChannelName("78"); got != want {
		t.Fatalf("label without group = %q, want %q", got, want)
	}
	if got := ChannelLabel(nil); got != "" {
		t.Fatalf("nil channel label = %q", got)
	}
}
