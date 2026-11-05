package environments_test

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/craggy-io/environment-services/environments"
)

// Open is the one assembly root: it opens the state directory,
// registers the host adapter always (docker/Android by presence and
// configuration) and hands back the whole lifecycle surface as a
// Manager. This example lists what a fresh manager starts with.
func ExampleOpen() {
	dir, err := os.MkdirTemp("", "environments-example-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	mgr, err := environments.Open(ctx, environments.Config{Dir: dir, ManagerID: "mgr_example"})
	if err != nil {
		panic(err)
	}
	defer mgr.Close(ctx, environments.ShutdownDisconnect)

	var profiles []string
	for _, p := range mgr.StockProfiles() {
		profiles = append(profiles, p.Name)
	}
	sort.Strings(profiles)
	fmt.Println("stock profiles:", profiles)

	hasHost := false
	for _, d := range mgr.Drivers() {
		if d.Name == "hostenv" {
			hasHost = true
		}
	}
	fmt.Println("host driver registered:", hasHost)
	fmt.Println("host-tools ready:", mgr.ProfileStatus("host-tools").Ready)
	// Output:
	// stock profiles: [android android-play android-play-rooted host-tools linux-tools]
	// host driver registered: true
	// host-tools ready: true
}
