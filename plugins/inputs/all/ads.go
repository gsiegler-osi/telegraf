//go:build !custom || inputs || inputs.ads

package all

import _ "github.com/influxdata/telegraf/plugins/inputs/ads" // register plugin
