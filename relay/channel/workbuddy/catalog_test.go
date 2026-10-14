package workbuddy

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCatalogModels(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{
			name: "剔除占位 id 与重复项并保序",
			ids:  []string{"default", "hy4-preview", "glm-5.3", "hy4-preview", "", "  glm-5.3-flash  "},
			want: []string{"hy4-preview", "glm-5.3", "glm-5.3-flash"},
		},
		{name: "空输入返回空切片", ids: nil, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCatalogModels(tt.ids)
			require.NotNil(t, got)
			assert.True(t, reflect.DeepEqual(tt.want, got), "want %v, got %v", tt.want, got)
		})
	}
}
