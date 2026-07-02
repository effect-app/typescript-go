package declarations

import (
	"testing"

	"github.com/microsoft/typescript-go/internal/packagejson"
)

func TestPackageJsonReferencesEffectApp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "effect-app dependency",
			json: `{"dependencies":{"effect-app":"latest"}}`,
			want: true,
		},
		{
			name: "effect-app scoped dependency",
			json: `{"devDependencies":{"@effect-app/core":"latest"}}`,
			want: true,
		},
		{
			name: "effect without effect-app",
			json: `{"dependencies":{"effect":"latest"}}`,
			want: false,
		},
		{
			name: "empty package",
			json: `{}`,
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fields, err := packagejson.Parse([]byte(test.json))
			if err != nil {
				t.Fatal(err)
			}
			got := packageJsonReferencesEffectApp(&packagejson.PackageJson{Fields: fields})
			if got != test.want {
				t.Fatalf("packageJsonReferencesEffectApp() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPackageJsonReferencesEffectAppNil(t *testing.T) {
	t.Parallel()

	if packageJsonReferencesEffectApp(nil) {
		t.Fatal("nil package.json should not reference effect-app")
	}
}
