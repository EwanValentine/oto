package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/doc"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/fatih/structtag"
	"github.com/pkg/errors"
	"golang.org/x/tools/go/packages"
)

var errNotFound = errors.New("not found")

// Definition describes an Oto definition.
type Definition struct {
	// PackageName is the name of the package.
	PackageName string `json:"packageName"`
	// Services are the services described in this definition.
	Services []Service `json:"services"`
	// Objects are the structures that are used throughout this definition.
	Objects []Object `json:"objects"`
	// Imports is a map of Go imports that should be imported into
	// Go code.
	Imports map[string]string `json:"imports"`
}

// Object looks up an object by name. Returns errNotFound error
// if it cannot find it.
func (d *Definition) Object(name string) (*Object, error) {
	for i := range d.Objects {
		obj := &d.Objects[i]
		if obj.Name == name {
			return obj, nil
		}
	}
	return nil, errNotFound
}

// Service describes a service, akin to an interface in Go.
type Service struct {
	Name    string   `json:"name"`
	Methods []Method `json:"methods"`
	Comment string   `json:"comment"`
}

// Method describes a method that a Service can perform.
type Method struct {
	Name           string    `json:"name"`
	NameLowerCamel string    `json:"nameLowerCamel"`
	InputObject    FieldType `json:"inputObject"`
	OutputObject   FieldType `json:"outputObject"`
	Comment        string    `json:"comment"`
}

// Object describes a data structure that is part of this definition.
type Object struct {
	TypeID   string  `json:"typeID"`
	Name     string  `json:"name"`
	Imported bool    `json:"imported"`
	Fields   []Field `json:"fields"`
	Comment  string  `json:"comment"`
}

// Field describes the field inside an Object.
type Field struct {
	Name           string              `json:"name"`
	NameLowerCamel string              `json:"nameLowerCamel"`
	Type           FieldType           `json:"type"`
	OmitEmpty      bool                `json:"omitEmpty"`
	Comment        string              `json:"comment"`
	Tag            string              `json:"tag"`
	ParsedTags     map[string]FieldTag `json:"parsedTags"`
	Example        interface{}         `json:"example"`
}

// FieldTag is a parsed tag.
// For more information, see Struct Tags in Go.
type FieldTag struct {
	// Value is the value of the tag.
	Value string `json:"value"`
	// Options are the options for the tag.
	Options []string `json:"options"`
}

// FieldType holds information about the type of data that this
// Field stores.
type FieldType struct {
	TypeID               string `json:"typeID"`
	TypeName             string `json:"typeName"`
	ObjectName           string `json:"objectName"`
	ObjectNameLowerCamel string `json:"objectNameLowerCamel"`
	Multiple             bool   `json:"multiple"`
	Package              string `json:"package"`
	IsObject             bool   `json:"isObject"`
	JSType               string `json:"jsType"`
}

type parser struct {
	Verbose bool

	ExcludeInterfaces []string

	patterns []string
	def      Definition

	// outputObjects marks output object names.
	outputObjects map[string]struct{}
	// objects marks object names.
	objects map[string]struct{}

	// docs are the docs for extracting comments.
	docs *doc.Package
}

// newParser makes a fresh parser using the specified patterns.
// The patterns should be the args passed into the tool (after any flags)
// and will be passed to the underlying build system.
func newParser(patterns ...string) *parser {
	return &parser{
		patterns: patterns,
	}
}

func (p *parser) parse() (Definition, error) {
	cfg := &packages.Config{
		Mode:  packages.NeedTypes | packages.NeedDeps | packages.NeedName | packages.NeedSyntax,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, p.patterns...)
	if err != nil {
		return p.def, err
	}
	p.outputObjects = make(map[string]struct{})
	p.objects = make(map[string]struct{})
	var excludedObjectsTypeIDs []string
	for _, pkg := range pkgs {
		p.docs, err = doc.NewFromFiles(pkg.Fset, pkg.Syntax, "")
		if err != nil {
			panic(err)
		}

		p.def.PackageName = pkg.Name
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			switch item := obj.Type().Underlying().(type) {
			case *types.Interface:
				s, err := p.parseService(pkg, obj, item)
				if err != nil {
					return p.def, err
				}
				if isInSlice(p.ExcludeInterfaces, name) {
					for _, method := range s.Methods {
						excludedObjectsTypeIDs = append(excludedObjectsTypeIDs, method.InputObject.TypeID)
						excludedObjectsTypeIDs = append(excludedObjectsTypeIDs, method.OutputObject.TypeID)
					}
					continue
				}
				p.def.Services = append(p.def.Services, s)
			case *types.Struct:
				p.parseObject(pkg, obj, item)
			}
		}
	}
	// remove any excluded objects
	nonExcludedObjects := make([]Object, 0, len(p.def.Objects))
	for _, object := range p.def.Objects {
		excluded := false
		for _, excludedTypeID := range excludedObjectsTypeIDs {
			if object.TypeID == excludedTypeID {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		nonExcludedObjects = append(nonExcludedObjects, object)
	}
	p.def.Objects = nonExcludedObjects
	sort.Slice(p.def.Services, func(i, j int) bool {
		return p.def.Services[i].Name < p.def.Services[j].Name
	})
	if err := p.addOutputFields(); err != nil {
		return p.def, err
	}
	return p.def, nil
}

func (p *parser) parseService(pkg *packages.Package, obj types.Object, interfaceType *types.Interface) (Service, error) {
	var s Service
	s.Name = obj.Name()
	s.Comment = p.commentForType(s.Name)
	if p.Verbose {
		fmt.Printf("%s ", s.Name)
	}
	l := interfaceType.NumMethods()
	for i := 0; i < l; i++ {
		m := interfaceType.Method(i)
		method, err := p.parseMethod(pkg, s.Name, m)
		if err != nil {
			return s, err
		}
		s.Methods = append(s.Methods, method)
	}
	return s, nil
}

func (p *parser) parseMethod(pkg *packages.Package, serviceName string, methodType *types.Func) (Method, error) {
	var m Method
	m.Name = methodType.Name()
	m.NameLowerCamel = camelizeDown(m.Name)
	m.Comment = p.commentForMethod(serviceName, m.Name)
	sig := methodType.Type().(*types.Signature)
	inputParams := sig.Params()
	if inputParams.Len() != 1 {
		return m, p.wrapErr(errors.New("invalid method signature: expected Method(MethodRequest) MethodResponse"), pkg, methodType.Pos())
	}
	var err error
	m.InputObject, err = p.parseFieldType(pkg, inputParams.At(0))
	if err != nil {
		return m, errors.Wrap(err, "parse input object type")
	}
	outputParams := sig.Results()
	if outputParams.Len() != 1 {
		return m, p.wrapErr(errors.New("invalid method signature: expected Method(MethodRequest) MethodResponse"), pkg, methodType.Pos())
	}
	m.OutputObject, err = p.parseFieldType(pkg, outputParams.At(0))
	if err != nil {
		return m, errors.Wrap(err, "parse output object type")
	}
	p.outputObjects[m.OutputObject.TypeName] = struct{}{}
	return m, nil
}

// parseObject parses a struct type and adds it to the Definition.
func (p *parser) parseObject(pkg *packages.Package, o types.Object, v *types.Struct) error {
	var obj Object
	obj.Name = o.Name()
	obj.Comment = p.commentForType(obj.Name)
	if _, found := p.objects[obj.Name]; found {
		// if this has already been parsed, skip it
		return nil
	}
	if o.Pkg().Name() != pkg.Name {
		obj.Imported = true
	}
	typ := v.Underlying()
	st, ok := typ.(*types.Struct)
	if !ok {
		return p.wrapErr(errors.New(obj.Name+" must be a struct"), pkg, o.Pos())
	}
	obj.TypeID = o.Pkg().Path() + "." + obj.Name
	for i := 0; i < st.NumFields(); i++ {
		field, err := p.parseField(pkg, obj.Name, st.Field(i))
		if err != nil {
			return err
		}
		field.Tag = v.Tag(i)
		field.ParsedTags, err = p.parseTags(field.Tag)
		if err != nil {
			return errors.Wrap(err, "parse field tag")
		}
		obj.Fields = append(obj.Fields, field)
	}
	p.def.Objects = append(p.def.Objects, obj)
	p.objects[obj.Name] = struct{}{}
	return nil
}

func (p *parser) parseTags(tag string) (map[string]FieldTag, error) {
	tags, err := structtag.Parse(tag)
	if err != nil {
		return nil, err
	}
	fieldTags := make(map[string]FieldTag)
	for _, tag := range tags.Tags() {
		fieldTags[tag.Key] = FieldTag{
			Value:   tag.Name,
			Options: tag.Options,
		}
	}
	return fieldTags, nil
}

func (p *parser) parseField(pkg *packages.Package, objectName string, v *types.Var) (Field, error) {
	var f Field
	f.Name = v.Name()
	f.NameLowerCamel = camelizeDown(f.Name)
	f.Comment = p.commentForField(objectName, f.Name)
	if !v.Exported() {
		return f, p.wrapErr(errors.New(f.Name+" must be exported"), pkg, v.Pos())
	}
	var err error
	f.Example, f.Comment, err = extractExample(f.Comment)
	if err != nil {
		return f, p.wrapErr(errors.New("extract comment example"), pkg, v.Pos())
	}
	f.Type, err = p.parseFieldType(pkg, v)
	if err != nil {
		return f, errors.Wrap(err, "parse type")
	}
	return f, nil
}

func (p *parser) parseFieldType(pkg *packages.Package, obj types.Object) (FieldType, error) {
	var ftype FieldType
	pkgPath := pkg.PkgPath
	resolver := func(other *types.Package) string {
		if other.Name() != pkg.Name {
			if p.def.Imports == nil {
				p.def.Imports = make(map[string]string)
			}
			p.def.Imports[other.Path()] = other.Name()
			ftype.Package = other.Path()
			pkgPath = other.Path()
			return other.Name()
		}
		return "" // no package prefix
	}
	typ := obj.Type()
	if slice, ok := obj.Type().(*types.Slice); ok {
		typ = slice.Elem()
		ftype.Multiple = true
	}
	if named, ok := typ.(*types.Named); ok {
		if structure, ok := named.Underlying().(*types.Struct); ok {
			if err := p.parseObject(pkg, named.Obj(), structure); err != nil {
				return ftype, err
			}
			ftype.IsObject = true
		}
	}
	ftype.TypeName = types.TypeString(typ, resolver)
	ftype.ObjectName = types.TypeString(typ, func(other *types.Package) string { return "" })
	ftype.ObjectNameLowerCamel = camelizeDown(ftype.ObjectName)
	ftype.TypeID = pkgPath + "." + ftype.ObjectName
	if ftype.IsObject {
		ftype.JSType = "object"
	} else {
		switch ftype.TypeName {
		case "interface{}":
			ftype.JSType = "any"
		case "map[string]interface{}":
			ftype.JSType = "object"
		case "string":
			ftype.JSType = "string"
		case "bool":
			ftype.JSType = "boolean"
		case "int", "int16", "int32", "int64",
			"uint", "uint16", "uint32", "uint64",
			"float32", "float64":
			ftype.JSType = "number"
		}
	}

	return ftype, nil
}

// addOutputFields adds built-in fields to the response objects
// mentioned in p.outputObjects.
func (p *parser) addOutputFields() error {
	errorField := Field{
		OmitEmpty:      true,
		Name:           "Error",
		NameLowerCamel: "error",
		Comment:        "Error is string explaining what went wrong. Empty if everything was fine.",
		Type: FieldType{
			TypeName: "string",
			JSType:   "string",
		},
	}
	for typeName := range p.outputObjects {
		obj, err := p.def.Object(typeName)
		if err != nil {
			// skip if we can't find it - it must be excluded
			continue
		}
		obj.Fields = append(obj.Fields, errorField)
	}
	return nil
}

func (p *parser) wrapErr(err error, pkg *packages.Package, pos token.Pos) error {
	position := pkg.Fset.Position(pos)
	return errors.Wrap(err, position.String())
}

func isInSlice(slice []string, s string) bool {
	for i := range slice {
		if slice[i] == s {
			return true
		}
	}
	return false
}

func (p *parser) lookupType(name string) *doc.Type {
	for i := range p.docs.Types {
		if p.docs.Types[i].Name == name {
			return p.docs.Types[i]
		}
	}
	return nil
}

func (p *parser) commentForType(name string) string {
	typ := p.lookupType(name)
	if typ == nil {
		return ""
	}
	return cleanComment(typ.Doc)
}

func (p *parser) commentForMethod(service, method string) string {
	typ := p.lookupType(service)
	if typ == nil {
		return ""
	}
	spec, ok := typ.Decl.Specs[0].(*ast.TypeSpec)
	if !ok {
		return ""
	}
	iface, ok := spec.Type.(*ast.InterfaceType)
	if !ok {
		return ""
	}
	var m *ast.Field
outer:
	for i := range iface.Methods.List {
		for _, name := range iface.Methods.List[i].Names {
			if name.Name == method {
				m = iface.Methods.List[i]
				break outer
			}
		}
	}
	if m == nil {
		return ""
	}
	return cleanComment(m.Doc.Text())
}

func (p *parser) commentForField(typeName, field string) string {
	typ := p.lookupType(typeName)
	if typ == nil {
		return ""
	}
	spec, ok := typ.Decl.Specs[0].(*ast.TypeSpec)
	if !ok {
		return ""
	}
	obj, ok := spec.Type.(*ast.StructType)
	if !ok {
		return ""
	}
	var f *ast.Field
outer:
	for i := range obj.Fields.List {
		for _, name := range obj.Fields.List[i].Names {
			if name.Name == field {
				f = obj.Fields.List[i]
				break outer
			}
		}
	}
	if f == nil {
		return ""
	}
	return cleanComment(f.Doc.Text())
}

func cleanComment(s string) string {
	return strings.TrimSpace(s)
}

// extractExample extracts the example from the comment.
// It returns a typed example, and the remaining
// comment string.
// The example should be on the last line.
func extractExample(comment string) (interface{}, string, error) {
	var lines []string
	const exampleCommentPrefix = "example:"
	s := bufio.NewScanner(strings.NewReader(comment))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, exampleCommentPrefix) {
			line = strings.TrimSpace(strings.TrimPrefix(line, exampleCommentPrefix))
			if line == "" {
				return nil, strings.Join(lines, "\n"), nil
			}
			var val interface{}
			if err := json.Unmarshal([]byte(line), &val); err != nil {
				return nil, "", err
			}
			return val, strings.Join(lines, "\n"), nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return nil, strings.Join(lines, "\n"), nil
}
