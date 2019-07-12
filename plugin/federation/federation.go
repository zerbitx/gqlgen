package federation

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/99designs/gqlgen/codegen"
	"github.com/99designs/gqlgen/codegen/config"
	"github.com/99designs/gqlgen/codegen/templates"
	"github.com/99designs/gqlgen/gqlfmt"
	"github.com/99designs/gqlgen/plugin"
	"github.com/vektah/gqlparser"
	"github.com/vektah/gqlparser/ast"
)

type federation struct{}

// New returns a federation plugin that injects
// federated directives and types into the schema
func New() plugin.Plugin {
	return &federation{}
}

func (f *federation) Name() string {
	return "federation"
}

func (f *federation) MutateConfig(cfg *config.Config) error {
	builtins := config.TypeMap{
		"_Service": {
			Model: config.StringList{
				"github.com/99designs/gqlgen/graphql/introspection.Service",
			},
		},
		"_Any": {Model: config.StringList{"github.com/99designs/gqlgen/graphql.Map"}},
	}
	for typeName, entry := range builtins {
		if cfg.Models.Exists(typeName) {
			return fmt.Errorf("%v already exists which must be reserved when Federation is enabled", typeName)
		}
		cfg.Models[typeName] = entry
	}

	return nil
}

func (f *federation) InjectSources() []*ast.Source {
	return []*ast.Source{f.getSource(false)}
}

func (f *federation) getSource(builtin bool) *ast.Source {
	return &ast.Source{
		Name: "federation.graphql",
		Input: `# Declarations as required by the federation spec 
# See: https://www.apollographql.com/docs/apollo-server/federation/federation-spec/

scalar _Any
scalar _FieldSet

directive @external on FIELD_DEFINITION
directive @requires(fields: _FieldSet!) on FIELD_DEFINITION
directive @provides(fields: _FieldSet!) on FIELD_DEFINITION
directive @key(fields: _FieldSet!) on OBJECT | INTERFACE
directive @extends on OBJECT
`,
		BuiltIn: builtin,
	}
}

func (f *federation) GenerateCode(data *codegen.Data) error {
	sdl, err := f.getSDL(data.Config)
	if err != nil {
		return err
	}
	return templates.Render(templates.Options{
		Template:        fmt.Sprintf(tmpl, sdl),
		PackageName:     data.Config.Exec.Package,
		Filename:        "service.go",
		Data:            data,
		GeneratedHeader: true,
	})
}

func (f *federation) getSDL(c *config.Config) (string, error) {
	sources := []*ast.Source{f.getSource(true)}
	for _, filename := range c.SchemaFilename {
		filename = filepath.ToSlash(filename)
		var err error
		var schemaRaw []byte
		schemaRaw, err = ioutil.ReadFile(filename)
		if err != nil {
			fmt.Fprintln(os.Stderr, "unable to open schema: "+err.Error())
			os.Exit(1)
		}
		sources = append(sources, &ast.Source{Name: filename, Input: string(schemaRaw)})
	}
	schema, err := gqlparser.LoadSchema(sources...)
	if err != nil {
		return "", err
	}
	return gqlfmt.PrintSchema(schema)
}

var tmpl = `
{{ reserveImport "context"  }}
{{ reserveImport "errors"  }}

{{ reserveImport "github.com/99designs/gqlgen/graphql/introspection" }}

func (ec *executionContext) __resolve__service(ctx context.Context) (introspection.Service, error) {
	if ec.DisableIntrospection {
		return introspection.Service{}, errors.New("federated introspection disabled")
	}
	return introspection.Service{
		SDL: ` + "`%s`" + `,
	}, nil
}`
