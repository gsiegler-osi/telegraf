package ads

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	goads "github.com/expo21xx/go-ads"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
)

type SymbolConfig struct {
	Name        string `toml:"name"`
	Key         string `toml:"key"`
	DisplayName string `toml:"display_name"`
	Unit        string `toml:"unit"`
}

type ADS struct {
	IP          string         `toml:"ip"`
	NetID       string         `toml:"netid"`
	Port        int            `toml:"port"`
	SourceNetID string         `toml:"source_netid"`
	Symbols     []SymbolConfig `toml:"symbol"`

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

  [[inputs.ads.symbol]]
    name = "MAIN.Temperature"
    key = "temperature"`
}

func (a *ADS) Start(acc telegraf.Accumulator) error {
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
		err := loadSingleSymbol(ctx, a.client, sym.Name)
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

	for _, sym := range a.Symbols {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		val, err := a.client.ReadByName(ctx, sym.Name)
		cancel()

		if err != nil {
			acc.AddError(fmt.Errorf("error reading symbol %s: %v", sym, err))
			continue
		}
		fieldKey := sym.Key
		if fieldKey == "" {
			fieldKey = sym.Name
		}

		tags := map[string]string{
			"netid": a.NetID,
			"ip":    a.IP,
		}

		if sym.DisplayName != "" {
			tags["display_name"] = sym.DisplayName
		}
		if sym.Unit != "" {
			tags["unit"] = sym.Unit
		}
		if sym.Key != "" {
			tags["plc_symbol"] = sym.Name
		}

		fields := map[string]interface{}{
			fieldKey: val,
		}

		acc.AddFields("ads", fields, tags)
	}

	return nil
}

func normalizeType(rawType string, size uint32) string {
	switch rawType {
	case "BOOL", "BYTE", "USINT", "SINT", "UINT", "WORD", "UDINT", "DWORD", "INT", "DINT", "REAL", "LREAL", "TIME", "DT", "TOD":
		return rawType
	}

	if strings.HasPrefix(rawType, "STRING") || strings.HasPrefix(rawType, "WSTRING") {
		return rawType
	}

	// Alias detection: If the custom type name contains "String" (e.g. T_MaxString)
	if strings.Contains(strings.ToUpper(rawType), "STRING") {
		return "STRING"
	}

	// Enum fallback: Map unknown types to integers based on their byte size
	switch size {
	case 1:
		return "USINT"
	case 2:
		return "INT" // Most TwinCAT enums evaluate to a 2-byte INT
	case 4:
		return "DINT" // Some large enums evaluate to a 4-byte DINT
	}

	return rawType // Return raw type if we can't guess it
}

func loadSingleSymbol(ctx context.Context, client *goads.Client, name string) error {
	reqData := make([]byte, 512)
	copy(reqData, name)

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

	relOffset := 30 + nameLen + 1
	if len(respData) < relOffset+typeLen {
		return fmt.Errorf("malformed type data length for %s", name)
	}

	rawType := string(respData[relOffset : relOffset+typeLen])

	// Apply the type normalizer to the incoming TwinCAT type
	symbol.Type = normalizeType(rawType, symbol.Size)
	symbol.Name = name

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
