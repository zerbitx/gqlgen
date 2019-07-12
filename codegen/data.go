package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/99designs/gqlgen/codegen/config"
	"github.com/99designs/gqlgen/gqlfmt"
	"github.com/pkg/errors"
	"github.com/vektah/gqlparser/ast"
)

// Data is a unified model of the code to be generated. Plugins may modify this structure to do things like implement
// resolvers or directives automatically (eg grpc, validation)
type Data struct {
	Config          *config.Config
	Schema          *ast.Schema
	SchemaStr       map[string]string
	Directives      DirectiveList
	Objects         Objects
	Inputs          Objects
	Interfaces      map[string]*Interface
	ReferencedTypes map[string]*config.TypeReference
	ComplexityRoots map[string]*Object
	Entities        []*Entity
	SDL             string

	QueryRoot        *Object
	MutationRoot     *Object
	SubscriptionRoot *Object
}

// Entity represents a federated type
// that was declared in the GQL schema.
type Entity struct {
	Name      string
	FieldName string
	FieldType string
	Def       *ast.Definition
}

type builder struct {
	Config     *config.Config
	Schema     *ast.Schema
	SchemaStr  map[string]string
	Binder     *config.Binder
	Directives map[string]*Directive
}

func BuildData(cfg *config.Config) (*Data, error) {
	b := builder{
		Config: cfg,
	}

	var err error
	b.Schema, err = cfg.LoadSchema()
	if err != nil {
		return nil, err
	}

	err = cfg.Check()
	if err != nil {
		return nil, err
	}

	err = cfg.Autobind(b.Schema)
	if err != nil {
		return nil, err
	}

	cfg.InjectBuiltins(b.Schema)

	b.Binder, err = b.Config.NewBinder(b.Schema)
	if err != nil {
		return nil, err
	}

	b.Directives, err = b.buildDirectives()
	if err != nil {
		return nil, err
	}

	dataDirectives := make(map[string]*Directive)
	for name, d := range b.Directives {
		if !d.Builtin {
			dataDirectives[name] = d
		}
	}

	s := Data{
		Config:     cfg,
		Directives: dataDirectives,
		Schema:     b.Schema,
		SchemaStr:  b.SchemaStr,
		Interfaces: map[string]*Interface{},
	}

	for _, schemaType := range b.Schema.Types {
		switch schemaType.Kind {
		case ast.Object:
			obj, err := b.buildObject(schemaType)
			if err != nil {
				return nil, errors.Wrap(err, "unable to build object definition")
			}

			s.Objects = append(s.Objects, obj)
			dir := schemaType.Directives.ForName("key") // TODO: interfaces
			if dir != nil {
				fieldName := dir.Arguments[0].Value.Raw // TODO: multiple arguments,a nd multiple keys
				s.Entities = append(s.Entities, &Entity{
					Name:      obj.Name,
					FieldName: fieldName,
					FieldType: obj.Fields[0].TypeReference.GO.String(),
					Def:       schemaType,
				})
			}
		case ast.InputObject:
			input, err := b.buildObject(schemaType)
			if err != nil {
				return nil, errors.Wrap(err, "unable to build input definition")
			}

			s.Inputs = append(s.Inputs, input)

		case ast.Union, ast.Interface:
			s.Interfaces[schemaType.Name] = b.buildInterface(schemaType)
		}
	}

	if s.Schema.Query != nil {
		s.QueryRoot = s.Objects.ByName(s.Schema.Query.Name)
	} else {
		return nil, fmt.Errorf("query entry point missing")
	}

	if s.Schema.Mutation != nil {
		s.MutationRoot = s.Objects.ByName(s.Schema.Mutation.Name)
	}

	if s.Schema.Subscription != nil {
		s.SubscriptionRoot = s.Objects.ByName(s.Schema.Subscription.Name)
	}

	if err := b.injectIntrospectionRoots(&s); err != nil {
		return nil, err
	}

	if cfg.Federated {
		b.injectEntityUnion(&s)
		b.injectEntitiesQuery(&s)
		b.injectSDL(&s)
	}
	s.ReferencedTypes, err = b.buildTypes()
	if err != nil {
		return nil, err
	}

	sort.Slice(s.Objects, func(i, j int) bool {
		return s.Objects[i].Definition.Name < s.Objects[j].Definition.Name
	})

	sort.Slice(s.Inputs, func(i, j int) bool {
		return s.Inputs[i].Definition.Name < s.Inputs[j].Definition.Name
	})

	str, err := gqlfmt.PrintSchema(b.Schema)
	if err != nil {
		return nil, err
	}
	// TODO: fix this
	s.SDL = strings.Replace(str, "_entities(representations: [_Any!]!): [_Entity]!", "", 1)
	s.SchemaStr = map[string]string{"schema.graphql": str}

	return &s, nil
}

func (b *builder) injectSDL(s *Data) error {
	typeDef := &ast.Definition{
		Kind: ast.Object,
		Name: "_Service",
		Fields: ast.FieldList{
			&ast.FieldDefinition{
				Name: "sdl",
				Type: ast.NonNullNamedType("String", nil),
			},
		},
	}
	obj, err := b.buildObject(typeDef)
	if err != nil {
		return err
	}
	s.Objects = append(s.Objects, obj)
	s.Schema.Types["_Service"] = typeDef

	fieldDef := &ast.FieldDefinition{
		Name: "_service",
		Type: ast.NonNullNamedType("_Service", nil),
	}
	_service, err := b.buildField(s.QueryRoot, fieldDef)
	if err != nil {
		return err
	}
	s.QueryRoot.Fields = append(s.QueryRoot.Fields, _service)
	s.Schema.Query.Fields = append(s.Schema.Query.Fields, fieldDef)
	return nil
}

func (b *builder) injectEntitiesQuery(s *Data) error {
	fieldDef := &ast.FieldDefinition{
		Name: "_entities",
		Type: ast.NonNullListType(ast.NamedType("_Entity", nil), nil),
		Arguments: ast.ArgumentDefinitionList{
			{
				Name: "representations",
				Type: ast.NonNullListType(ast.NonNullNamedType("_Any", nil), nil),
			},
		},
	}
	_entities, err := b.buildField(s.QueryRoot, fieldDef)
	if err != nil {
		return err
	}
	s.QueryRoot.Fields = append(s.QueryRoot.Fields, _entities)
	s.Schema.Query.Fields = append(s.Schema.Query.Fields, fieldDef)
	return nil
}

func (b *builder) injectEntityUnion(s *Data) error {
	possibleTypes := []string{}
	defs := []*ast.Definition{}
	for _, e := range s.Entities {
		possibleTypes = append(possibleTypes, e.Name)
		defs = append(defs, e.Def)
	}
	union := &ast.Definition{
		Kind:  ast.Union,
		Name:  "_Entity",
		Types: possibleTypes,
	}

	s.Schema.PossibleTypes["_Entity"] = defs

	obj := b.buildInterface(union)
	s.Interfaces[union.Name] = obj
	s.Schema.Types[union.Name] = union
	for _, e := range s.Entities {
		for _, o := range s.Objects {
			if o.Name == e.Name {
				o.Implements = append(o.Implements, union)
			}
		}
	}
	return nil
}

func (b *builder) injectIntrospectionRoots(s *Data) error {
	obj := s.Objects.ByName(b.Schema.Query.Name)
	if obj == nil {
		return fmt.Errorf("root query type must be defined")
	}

	__type, err := b.buildField(obj, &ast.FieldDefinition{
		Name: "__type",
		Type: ast.NamedType("__Type", nil),
		Arguments: []*ast.ArgumentDefinition{
			{
				Name: "name",
				Type: ast.NonNullNamedType("String", nil),
			},
		},
	})
	if err != nil {
		return err
	}

	__schema, err := b.buildField(obj, &ast.FieldDefinition{
		Name: "__schema",
		Type: ast.NamedType("__Schema", nil),
	})
	if err != nil {
		return err
	}

	obj.Fields = append(obj.Fields, __type, __schema)

	return nil
}
