package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const bootstrapFile = "internal/bootstrap/modules.go"

// moduleFiles maps module skeleton templates to their paths below
// internal/modules/<name>.
var moduleFiles = []struct{ template, path string }{
	{"module/module.go.tmpl", "module.go"},
	{"module/build.go.tmpl", "build.go"},
	{"module/generate.go.tmpl", "generate.go"},
	{"module/tenant_purge.go.tmpl", "tenant_purge.go"},
	{"module/module_test.go.tmpl", "module_test.go"},
	{"module/README.md.tmpl", "README.md"},
	{"module/sqlc.yaml.tmpl", "sqlc.yaml"},
	{"module/catalog/catalog.go.tmpl", "catalog/catalog.go"},
	{"module/catalog/permissions.go.tmpl", "catalog/permissions.go"},
	{"module/catalog/errors.go.tmpl", "catalog/errors.go"},
	{"module/ports.go.tmpl", "service/ports.go"},
	{"module/authorize.go.tmpl", "service/authorize.go"},
	{"module/health.go.tmpl", "service/health.go"},
	{"module/health_test.go.tmpl", "service/health_test.go"},
	{"module/service_main_test.go.tmpl", "service/main_test.go"},
	{"module/repository_health.go.tmpl", "repository/health.go"},
	{"module/handler_health.go.tmpl", "handler/health.go"},
	{"module/handler_health_test.go.tmpl", "handler/health_test.go"},
	{"module/handler_main_test.go.tmpl", "handler/main_test.go"},
	{"module/dto.go.tmpl", "dto/dto.go"},
	{"module/health.sql.tmpl", "queries/health.sql"},
}

// moduleLocales are the skeleton's en/vi strings (keys in parity).
func moduleLocales(name string) map[string]map[string]string {
	key := "errors." + name + ".not_implemented.title"
	return map[string]map[string]string{
		"en": {key: "Not implemented"},
		"vi": {key: "Chưa được triển khai"},
	}
}

func (g *generator) generateModule(ctx context.Context, name string) error {
	p, err := g.planModule(name)
	if err != nil {
		return err
	}
	return g.execute(ctx, p, verifySteps(name, true), []string{
		"Add a resource: make gen resource=<name> module=" + name,
		"Apply the migration locally: make migrate-run",
		"Refresh the API spec: make openapi",
		"Grant roles deliberately in internal/modules/" + name + "/catalog/permissions.go (admin-only by default)",
		"Generated routes are huma operations with their policy; extend them at the gen: markers (see docs/module-template.md#3-route-wiring)",
	})
}

func (g *generator) planModule(name string) (*plan, error) {
	if err := validateModuleName(name); err != nil {
		return nil, err
	}
	dir := g.moduleDir(name)
	if exists, err := g.exists(dir); err != nil {
		return nil, err
	} else if exists {
		return nil, fmt.Errorf("%s already exists; refusing to overwrite it", dir)
	}
	data := templateData{M: newModuleData(g.modulePath, name)}
	p := &plan{repo: g.repo, regenerated: []string{dir + "/sqlc"}}
	for _, f := range moduleFiles {
		content, err := render(f.template, data)
		if err != nil {
			return nil, err
		}
		p.create(dir+"/"+f.path, content)
	}
	locales := moduleLocales(name)
	for _, locale := range []string{"en", "vi"} {
		content, err := marshalLocale(locales[locale])
		if err != nil {
			return nil, err
		}
		p.create(dir+"/catalog/locales/"+locale+".json", content)
	}
	version, err := g.nextMigrationVersion(nil)
	if err != nil {
		return nil, err
	}
	for _, direction := range []string{"up", "down"} {
		content, err := render("module/init."+direction+".sql.tmpl", data)
		if err != nil {
			return nil, err
		}
		p.create("migrations/"+version+"_"+name+"_init."+direction+".sql", content)
	}
	if err := g.editFile(p, bootstrapFile, func(src string) (string, error) {
		return registerInBootstrap(src, g.modulePath, name)
	}); err != nil {
		return nil, err
	}
	return p, nil
}

// registerInBootstrap adds the module's import, Build call, List entry, and
// tenant-purger registration to internal/bootstrap/modules.go. It is
// idempotent: a module whose import is already present is left untouched.
func registerInBootstrap(src, modulePath, name string) (string, error) {
	importPath := modulePath + "/internal/modules/" + name
	if strings.Contains(src, strconv.Quote(importPath)) {
		return src, nil
	}
	out, err := ensureImports(bootstrapFile, src, importPath)
	if err != nil {
		return "", err
	}
	variable := camel(name) + "Module"
	build := fmt.Sprintf("%s, err := %s.Build(infra, %s.Settings{}, %s.Deps{})\nif err != nil {\n\treturn nil, err\n}", variable, name, name, name)
	if out, err = insertAtMarker(bootstrapFile, out, markerBootstrapBuild, build); err != nil {
		return "", err
	}
	if out, err = insertAtMarker(bootstrapFile, out, markerBootstrapPurge, variable+","); err != nil {
		return "", err
	}
	return insertAtMarker(bootstrapFile, out, markerBootstrapList, variable+",")
}
