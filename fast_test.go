package toml_test

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
)

func TestFastSimple(t *testing.T) {
	m := map[string]int64{}
	err := toml.Unmarshal([]byte(`a = 42`), &m)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"a": 42}, m)
}

func TestFastSimpleString(t *testing.T) {
	m := map[string]string{}
	err := toml.Unmarshal([]byte(`a = "hello"`), &m)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"a": "hello"}, m)
}

func TestFastSimpleInterface(t *testing.T) {
	m := map[string]interface{}{}
	err := toml.Unmarshal([]byte(`
	a = "hello"
	b = 42`), &m)
	require.NoError(t, err)
	require.Equal(t, map[string]interface{}{
		"a": "hello",
		"b": int64(42),
	}, m)
}

func TestFastMultipartKeyInterface(t *testing.T) {
	m := map[string]interface{}{}
	err := toml.Unmarshal([]byte(`
	a.b.c = "hello"
	b = 42`), &m)
	require.NoError(t, err)
	require.Equal(t, map[string]interface{}{
		"a": map[string]interface{}{
			"b": map[string]interface{}{
				"c": "hello",
			},
		},
		"b": int64(42),
	}, m)
}
