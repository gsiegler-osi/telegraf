package ads

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	goads "github.com/expo21xx/go-ads"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
)

type ADS struct {
	IP          string   `toml:"ip"`
	NetID       string   `toml:"netid"`
	Port        int      `toml:"port"`
	SourceNetID string   `toml:"source_netid"`
	Symbols     []string `toml:"symbols"`

	client *goads.Client
}

func (a *ADS) Description() string {
	return "Reads variables from Beckhoff TwinCAT via ADS"
}

func (a *ADS) SampleConfig() string {
	return `
  ip = "127.0.0.1"
  netid = "192.168.1.10.1.1"
  port = 851
  source_netid = "192.168.1.10.1.3"
  symbols = [
    "MAIN.Temperature"
  ]
`
}

func (a *ADS) Start(acc telegraf.Accumulator) error {
	// NO symbol loading options. We will do it manually.
	opts := []goads.Option{}

	if a.SourceNetID != "" {
		srcNetID, err := goads.ParseNetIDFromString(a.SourceNetID)
		if err != nil {
			return fmt.Errorf("invalid source_netid: %w", err)
		}
		opts = append(opts, goads.WithSourceNetID(srcNetID))
	}

	client, err := goads.NewClient(a.IP, a.NetID, a.Port, opts...)
	if err != nil {
		return fmt.Errorf("failed to create ADS client: %w", err)
	}
	a.client = client

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = a.client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to ADS server: %w", err)
	}

	// Look up only the symbols specifically requested in telegraf.conf
	for _, sym := range a.Symbols {
		err := loadSingleSymbol(ctx, a.client, sym)
		if err != nil {
			// Add an error so Telegraf logs it, but continue trying to load the rest
			acc.AddError(fmt.Errorf("failed to load symbol info for %s: %w", sym, err))
		}
	}

	return nil
}

func (a *ADS) Stop() {
	if a.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.client.Close(ctx)
	}
}

func (a *ADS) Gather(acc telegraf.Accumulator) error {
	if a.client == nil {
		return fmt.Errorf("ADS client is not connected")
	}

	fields := make(map[string]interface{})

	for _, sym := range a.Symbols {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		val, err := a.client.ReadByName(ctx, sym)
		cancel()

		if err != nil {
			acc.AddError(fmt.Errorf("error reading symbol %s: %v", sym, err))
			continue
		}

		fields[sym] = val
	}

	if len(fields) > 0 {
		tags := map[string]string{
			"netid": a.NetID,
			"ip":    a.IP,
		}
		acc.AddFields("ads", fields, tags)
	}

	return nil
}

// loadSingleSymbol fetches the memory address for a specific variable directly
func loadSingleSymbol(ctx context.Context, client *goads.Client, name string) error {
	// PADDING TRICK: The library bounds ReadLength to the length of the data we send.
	// We pad the string with zeros out to 512 bytes so TwinCAT has enough room
	// to return the full symbol metadata struct.
	reqData := make([]byte, 512)
	copy(reqData, name)

	// ADSIndexGroupSymInfByNameEx is 0xF009
	respData, err := client.ReadWrite(ctx, 0xF009, 0, reqData)
	if err != nil {
		return err
	}

	if len(respData) < 30 {
		return fmt.Errorf("response data too short for symbol %s", name)
	}

	var symbol goads.Symbol
	symbol.IndexGroup = binary.LittleEndian.Uint32(respData[4:8])
	symbol.IndexOffset = binary.LittleEndian.Uint32(respData[8:12])
	symbol.Size = binary.LittleEndian.Uint32(respData[12:16])
	symbol.Flags = binary.LittleEndian.Uint32(respData[20:24])

	nameLen := int(binary.LittleEndian.Uint16(respData[24:26]))
	typeLen := int(binary.LittleEndian.Uint16(respData[26:28]))

	// Extract the TwinCAT Type (e.g., "BOOL", "LREAL", "INT")
	relOffset := 30 + nameLen + 1
	if len(respData) < relOffset+typeLen {
		return fmt.Errorf("malformed type data length for %s", name)
	}

	symbol.Type = string(respData[relOffset : relOffset+typeLen])
	symbol.Name = name // Force it to perfectly match your Telegraf config casing

	// Register it with the library so Gather() can read it
	client.AddSymbol(symbol)

	return nil
}

func init() {
	inputs.Add("ads", func() telegraf.Input {
		return &ADS{
			Port: 851,
		}
	})
}
