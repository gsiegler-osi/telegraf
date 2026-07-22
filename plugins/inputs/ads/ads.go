package ads

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	goads "github.com/gsiegler-osi/go-ads"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
)

type SymbolConfig struct {
	Name    string            `toml:"name"`
	Address string            `toml:"address"`
	Tags    map[string]string `toml:"tags"`

	dataType string
}

type ADS struct {
	IP             string `toml:"ip"`
	NetID          string `toml:"netid"`
	Port           int    `toml:"port"`
	SourceNetID    string `toml:"source_netid"`
	SourcePort     int    `toml:"source_port"`
	WatchdogSymbol string `toml:"watchdog_symbol"`
	// TimeSymbol     string         `toml:"time_symbol"`
	Symbols []SymbolConfig `toml:"symbol"`

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
    address = "temperature"`
}

func (a *ADS) Start(acc telegraf.Accumulator) error {
	err := a.connect(acc)
	if err != nil {
		acc.AddError(fmt.Errorf("initial PLC connection failed, will retry on next gather: %v", err))
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
		if err := a.connect(acc); err != nil {
			acc.AddError(fmt.Errorf("reconnect attempt failed: %v", err))
			return nil // Safely abort this cycle and try again next time
		}
	}

	cycleTime := time.Now().UTC()
	// if a.TimeSymbol != "" {
	// 	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	// 	plcTime, err := a.client.ReadByName(ctx, a.TimeSymbol)
	// 	cancel()

	// 	if err != nil {
	// 		acc.AddError(fmt.Errorf("error reading symbol %s: %v", a.TimeSymbol, err))
	// 	} else {
	// 		cycleTime, err = convertTimestamp(plcTime)
	// 		if err != nil {
	// 			acc.AddError(fmt.Errorf("error converting timestamp: %v", err))
	// 		}
	// 	}
	// }

	for _, sym := range a.Symbols {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		val, err := a.client.ReadByName(ctx, sym.Address)
		cancel()

		if err != nil {
			acc.AddError(fmt.Errorf("error reading symbol %s: %v", sym.Address, err))

			errStr := strings.ToLower(err.Error())
			if strings.Contains(errStr, "eof") || strings.Contains(errStr, "writing") || strings.Contains(errStr, "connection") {
				a.client.Close(context.Background())
				a.client = nil
				acc.AddError(fmt.Errorf("PLC TCP connection dropped. Forcing a complete reconnect on next cycle."))
				return nil
			}
			continue
		}

		fieldKey := sym.Name
		if fieldKey == "" {
			fieldKey = sym.Address
		}

		tags := map[string]string{"netid": a.NetID, "ip": a.IP, "datatype": sym.dataType}
		if len(sym.Tags) != 0 {
			for k, v := range sym.Tags {
				tags[k] = v
			}
		}
		if sym.Name != "" {
			tags["name"] = sym.Name
		}
		tags["address"] = sym.Address

		fields := map[string]interface{}{fieldKey: val}

		acc.AddFields("ads", fields, tags, cycleTime)
	}

	// Watchdog Ping
	if a.WatchdogSymbol != "" && a.client != nil {
		go func(c *goads.Client) {
			wdCtx, wdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer wdCancel()

			err := c.WriteByName(wdCtx, a.WatchdogSymbol, []byte{1})
			if err != nil {
				acc.AddError(fmt.Errorf("failed to write watchdog heartbeat: %v", err))
			}
		}(a.client)
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

	if strings.Contains(strings.ToUpper(rawType), "STRING") {
		return "STRING"
	}

	switch size {
	case 1:
		return "USINT"
	case 2:
		return "INT"
	case 4:
		return "DINT"
	}

	return rawType
}

func loadSingleSymbol(ctx context.Context, client *goads.Client, name string) error {
	reqData := make([]byte, len(name)+1)
	copy(reqData, name)

	respData, err := client.ReadWriteWithLen(ctx, 0xF009, 0, reqData, 0xFFFF)
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

	symbol.Type = normalizeType(rawType, symbol.Size)
	symbol.Name = name

	client.AddSymbol(symbol)
	return nil
}

func convertTimestamp(ts any) (time.Time, error) {
	switch v := ts.(type) {

	case time.Time:
		return v, nil

	case uint32:
		return time.Unix(int64(v), 0), nil
	case int32:
		return time.Unix(int64(v), 0), nil

	case int64:
		return time.UnixMilli(v), nil
	case uint64:
		return time.UnixMilli(int64(v)), nil

	case float64:
		return time.UnixMilli(int64(v)), nil

	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err == nil {
			return t, nil
		}

		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.UnixMilli(ms), nil
		}

		return time.Time{}, fmt.Errorf("could not parse string timestamp: %s", v)

	default:
		return time.Time{}, fmt.Errorf("unsupported TwinCAT timestamp type: %T", v)
	}
}

func (a *ADS) connect(acc telegraf.Accumulator) error {
	opts := []goads.Option{}

	if a.SourceNetID != "" {
		srcNetID, err := goads.ParseNetIDFromString(a.SourceNetID)
		if err != nil {
			return fmt.Errorf("invalid source_netid: %w", err)
		}
		opts = append(opts, goads.WithSourceNetID(srcNetID))
	}

	srcPort := goads.NetPort(a.SourcePort)
	opts = append(opts, goads.WithSourceNetPort(srcPort))

	client, err := goads.NewClient(a.IP, a.NetID, a.Port, opts...)
	if err != nil {
		return fmt.Errorf("failed to create ADS client: %w", err)
	}

	a.client = client

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = a.client.Connect(ctx)
	if err != nil {
		a.client = nil
		return fmt.Errorf("failed to connect to ADS server: %w", err)
	}

	// Re-load all symbols to get fresh IndexOffsets in case the PLC recompiled/restarted
	for i := range a.Symbols {
		symCtx, symCancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := loadSingleSymbol(symCtx, a.client, a.Symbols[i].Address)
		symCancel()

		if err != nil {
			acc.AddError(fmt.Errorf("failed to load symbol info for %s: %w", a.Symbols[i].Address, err))
		} else {
			client_symbol, ok := a.client.GetSymbol(a.Symbols[i].Address)
			if ok {
				a.Symbols[i].dataType = client_symbol.Type
			}
		}
	}

	if a.WatchdogSymbol != "" {
		wdCtx, wdCancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := loadSingleSymbol(wdCtx, a.client, a.WatchdogSymbol)
		wdCancel()

		if err != nil {
			acc.AddError(fmt.Errorf("failed to load symbol info for %s: %w", a.WatchdogSymbol, err))
		}
	}

	// if a.TimeSymbol != "" {
	// 	wdCtx, wdCancel := context.WithTimeout(context.Background(), 2*time.Second)
	// 	err := loadSingleSymbol(wdCtx, a.client, a.TimeSymbol)
	// 	wdCancel()

	// 	if err != nil {
	// 		acc.AddError(fmt.Errorf("failed to load symbol info for %s: %w", a.TimeSymbol, err))
	// 	}
	// }

	return nil
}

func init() {
	inputs.Add("ads", func() telegraf.Input {
		return &ADS{
			Port:       851,
			SourcePort: 32750,
		}
	})
}
