// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the AGPLv3,
// which can be found in the LICENSE file or at gnu.org/licenses/agpl.html

// +build go_run_only

package main

import (
	"fmt"
	"os"

	"zgo.at/zpack"
)

func main() {
	err := zpack.Pack(map[string]map[string]string{
		"./pack/pack.go": map[string]string{
			"Public":           "./public",
			"Templates":        "./tpl",
			"SchemaSQLite":     "./db/schema.sql",
			"MigrationsPgSQL":  "./db/migrate/pgsql",
			"MigrationsSQLite": "./db/migrate/sqlite",
		},
	}, "/.keep", "public/fonts/LICENSE")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Don't need to commit this.
	if _, err := os.Stat("./GeoLite2-Country.mmdb"); err == nil {
		err := zpack.Pack(map[string]map[string]string{
			"./pack/geodb.go": map[string]string{
				"GeoDB": "./GeoLite2-Country.mmdb",
			},
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
