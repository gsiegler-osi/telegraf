package ads

import (
	"context"
	"testing"
	"time"

	goads "github.com/gsiegler-osi/go-ads"
	"github.com/influxdata/telegraf/testutil"
	"github.com/stretchr/testify/require"
)

type MockADSClient struct {
	ConnectCount          int
	ReadByNameCount       int
	WriteByNameCount      int
	ReadWriteWithLenCount int
	AddSymbolCount        int
	WriteSymbol           string
	WriteDone             chan struct{}

	FakeSymbols map[string]goads.Symbol
	FakeValues  map[string]interface{}
}

func (m *MockADSClient) Connect(ctx context.Context) error {
	m.ConnectCount++
	return nil
}

func (m *MockADSClient) Close(ctx context.Context) error {
	return nil
}

func (m *MockADSClient) ReadByName(ctx context.Context, name string) (interface{}, error) {
	m.ReadByNameCount++
	return m.FakeValues[name], nil
}

func (m *MockADSClient) WriteByName(ctx context.Context, name string, data []byte) error {
	m.WriteByNameCount++
	m.WriteSymbol = name

	if m.WriteDone != nil {
		m.WriteDone <- struct{}{}
	}

	return nil
}

func (m *MockADSClient) ReadWriteWithLen(ctx context.Context, ig uint32, io uint32, data []byte, rl uint32) ([]byte, error) {
	m.ReadWriteWithLenCount++
	return make([]byte, 64), nil
}

func (m *MockADSClient) AddSymbol(sym goads.Symbol) {
	m.AddSymbolCount++
	if m.FakeSymbols == nil {
		m.FakeSymbols = make(map[string]goads.Symbol)
	}
	m.FakeSymbols[sym.Name] = sym
}

func (m *MockADSClient) GetSymbol(name string) (goads.Symbol, bool) {
	sym, ok := m.FakeSymbols[name]
	return sym, ok
}

func TestSampleConfig(t *testing.T) {
	plugin := &ADS{}
	require.NotEmpty(t, plugin.SampleConfig())
}

func TestNormalizeType(t *testing.T) {
	tests := []struct {
		name     string
		rawType  string
		size     uint32
		expected string
	}{
		{
			name:     "Standard BOOL",
			rawType:  "BOOL",
			size:     1,
			expected: "BOOL",
		},
		{
			name:     "Standard REAL",
			rawType:  "REAL",
			size:     4,
			expected: "REAL",
		},
		{
			name:     "Explicit STRING fallback",
			rawType:  "STRING(80)",
			size:     80,
			expected: "STRING(80)",
		},
		{
			name:     "Implicit String Match",
			rawType:  "MY_CUSTOM_STRING_TYPE",
			size:     255,
			expected: "STRING",
		},
		{
			name:     "Size fallback to USINT",
			rawType:  "UNKNOWN_CUSTOM_ALIAS",
			size:     1,
			expected: "USINT",
		},
		{
			name:     "Size fallback to INT",
			rawType:  "UNKNOWN_CUSTOM_ALIAS",
			size:     2,
			expected: "INT",
		},
		{
			name:     "Size fallback to DINT",
			rawType:  "UNKNOWN_CUSTOM_ALIAS",
			size:     4,
			expected: "DINT",
		},
		{
			name:     "Unknown Type with arbitrary size returns raw",
			rawType:  "CUSTOM_STRUCT",
			size:     32,
			expected: "CUSTOM_STRUCT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeType(tt.rawType, tt.size)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestConvertTimestamp(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond).UTC()
	unixMs := now.UnixMilli()

	tests := []struct {
		name        string
		input       any
		expected    time.Time
		expectError bool
	}{
		{
			name:        "time.Time passthrough",
			input:       now,
			expected:    now,
			expectError: false,
		},
		{
			name:        "int64 milliseconds",
			input:       int64(unixMs),
			expected:    now,
			expectError: false,
		},
		{
			name:        "float64 milliseconds",
			input:       float64(unixMs),
			expected:    now,
			expectError: false,
		},
		{
			name:        "Unsupported type",
			input:       []byte{0x00, 0x01},
			expected:    time.Time{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := convertTimestamp(tt.input)
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected.UnixMilli(), result.UnixMilli())
			}
		})
	}
}

func TestStopWithoutConnect(t *testing.T) {
	plugin := &ADS{
		IP:          "127.0.0.1",
		NetID:       "192.168.1.10.1.1",
		Port:        851,
		SourceNetID: "127.0.0.1.1.2",
		SourcePort:  32750,
	}

	require.NotPanics(t, plugin.Stop)
}

func TestGatherUnconnected(t *testing.T) {
	plugin := &ADS{
		IP:          "999.999.999.999",
		NetID:       "192.168.1.10.1.1",
		Port:        851,
		SourceNetID: "127.0.0.1.1.2",
	}

	var acc testutil.Accumulator

	err := plugin.Start(&acc)
	require.NoError(t, err)

	err = plugin.Gather(&acc)
	require.NoError(t, err)

	require.NotEmpty(t, acc.Errors)
	require.Contains(t, acc.Errors[0].Error(), "connection failed")
}

func TestGatherWithMock(t *testing.T) {
	mock := &MockADSClient{
		FakeValues: map[string]interface{}{
			"MAIN.Temperature": float32(45.5),
		},
	}

	plugin := &ADS{
		TimeSymbol: "MAIN.Time",
		Symbols: []SymbolConfig{
			{Name: "temp", Address: "MAIN.Temperature"},
		},
		client: mock,
	}

	var acc testutil.Accumulator

	err := plugin.Gather(&acc)
	require.NoError(t, err)

	require.Equal(t, 2, mock.ReadByNameCount, "ReadByName should be called twice (TimeSymbol + 1 Symbol)")

	require.True(t, acc.HasPoint("ads", map[string]string{"netid": "", "ip": "", "datatype": "", "name": "temp", "address": "MAIN.Temperature"}, "temp", float32(45.5)))
}

func TestWriteWatchdogSymbol(t *testing.T) {
	mock := &MockADSClient{
		WriteDone: make(chan struct{}, 1),
	}

	plugin := &ADS{
		WatchdogSymbol: "MAIN.Watchdog",
		Symbols:        []SymbolConfig{},
		client:         mock,
	}

	var acc testutil.Accumulator

	err := plugin.Gather(&acc)
	require.NoError(t, err)

	select {
	case <-mock.WriteDone:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timeout waiting for async watchdog ping")
	}

	require.Equal(t, 1, mock.WriteByNameCount, "WriteByName should be called once")
	require.Equal(t, plugin.WatchdogSymbol, mock.WriteSymbol, "WriteByName should be called with the watchdog symbol")
}

func TestLoadSingleSymbolCalled(t *testing.T) {
	mock := &MockADSClient{}

	err := loadSingleSymbol(context.Background(), mock, "MAIN.TestVar")

	require.NoError(t, err)
	require.Equal(t, 1, mock.ReadWriteWithLenCount, "loadSingleSymbol failed to execute ReadWriteWithLen")
}
