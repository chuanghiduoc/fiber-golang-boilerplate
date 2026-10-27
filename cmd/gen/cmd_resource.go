package main

import (
	"context"
	"fmt"
)

func (g *generator) generateResource(ctx context.Context, module, name, pluralOverride string) error {
	p, data, err := g.planResource(module, name, pluralOverride)
	if err != nil {
		return err
	}
	return g.execute(ctx, p, verifySteps(module, true), []string{
		"Review the migration and the " + data.R.Table + " columns, then apply it: make migrate-run",
		"Run the generated integration test: go test -tags=integration ./internal/modules/" + module + "/...",
		"Grant roles deliberately in internal/modules/" + module + "/catalog/permissions.go (admin-only by default)",
		"Review the vi translation in catalog/locales/vi.json, then refresh the API spec: make openapi",
	})
}

func (g *generator) planResource(module, name, pluralOverride string) (*plan, templateData, error) {
	if err := g.requireGeneratedModule(module); err != nil {
		return nil, templateData{}, err
	}
	if err := validateResourceName(name); err != nil {
		return nil, templateData{}, err
	}
	pluralName := pluralSnake(name)
	if pluralOverride != "" {
		if !snakeName.MatchString(pluralOverride) || pluralOverride == name {
			return nil, templateData{}, fmt.Errorf("--plural %q must be lower_snake_case and differ from %q", pluralOverride, name)
		}
		pluralName = pluralOverride
	}
	data := templateData{M: newModuleData(g.modulePath, module), R: newResourceData(newModuleData(g.modulePath, module), name, pluralName)}
	dir := g.moduleDir(module)
	p := &plan{repo: g.repo, regenerated: []string{dir + "/sqlc"}}

	files := []struct{ template, path string }{
		{"resource/service.go.tmpl", "service/" + name + ".go"},
		{"resource/service_test.go.tmpl", "service/" + name + "_test.go"},
		{"resource/repository.go.tmpl", "repository/" + name + ".go"},
		{"resource/tenant_purge.go.tmpl", "repository/" + name + "_purge.go"},
		{"resource/handler.go.tmpl", "handler/" + name + ".go"},
		{"resource/handler_test.go.tmpl", "handler/" + name + "_test.go"},
		{"resource/dto.go.tmpl", "dto/" + name + ".go"},
		{"resource/queries.sql.tmpl", "queries/" + name + ".sql"},
		{"resource/routes.go.tmpl", "routes_" + name + ".go"},
		{"resource/integration_test.go.tmpl", name + "_integration_test.go"},
	}
	// The pgx.ErrNoRows mapping is shared by every resource of the module.
	if exists, err := g.exists(dir + "/repository/errors.go"); err != nil {
		return nil, templateData{}, err
	} else if !exists {
		files = append(files, struct{ template, path string }{"resource/repository_errors.go.tmpl", "repository/errors.go"})
	}
	for _, f := range files {
		content, err := render(f.template, data)
		if err != nil {
			return nil, templateData{}, err
		}
		p.create(dir+"/"+f.path, content)
	}

	version, err := g.nextMigrationVersion(nil)
	if err != nil {
		return nil, templateData{}, err
	}
	for _, direction := range []string{"up", "down"} {
		content, err := render("resource/migration."+direction+".sql.tmpl", data)
		if err != nil {
			return nil, templateData{}, err
		}
		p.create("migrations/"+version+"_"+module+"_create_"+pluralName+"."+direction+".sql", content)
	}

	if err := g.wireResource(p, data); err != nil {
		return nil, templateData{}, err
	}
	return p, data, nil
}

// wireResource extends the module's existing files at their gen: markers.
func (g *generator) wireResource(p *plan, data templateData) error {
	m, r := data.M, data.R
	dir := g.moduleDir(m.Name)

	port, err := render("resource/port.go.tmpl", data)
	if err != nil {
		return err
	}
	edits := []struct {
		file string
		edit func(string) (string, error)
	}{
		{"service/ports.go", func(src string) (string, error) {
			return ensureImports("service/ports.go", src+string(port), "context", "time", "github.com/google/uuid", m.ModulePath+"/pkg/pagination")
		}},
		{"module.go", func(src string) (string, error) {
			out, err := insertAtMarker("module.go", src, markerHandlers,
				fmt.Sprintf("%s *%shandler.%sHandler", r.PluralPascal, m.Ident, r.Pascal))
			if err != nil {
				return "", err
			}
			return insertAtMarker("module.go", out, markerRoutes, fmt.Sprintf("m.register%sRoutes(api)", r.Pascal))
		}},
		{"build.go", func(src string) (string, error) {
			out, err := insertAtMarker("build.go", src, markerBuild, fmt.Sprintf(
				"%s: %shandler.New%sHandler(%sservice.New%sService(%srepository.New%sRepository(infra.DB))),",
				r.PluralPascal, m.Ident, r.Pascal, m.Ident, r.Pascal, m.Ident, r.Pascal))
			if err != nil {
				return "", err
			}
			// Every resource table is tenant-owned, so it needs a purge path.
			// Repositories take infra.DB (the RLS-scoped handle), never the raw
			// bypass pool.
			return insertAtMarker("build.go", out, markerPurge, fmt.Sprintf(
				"m.tenantPurgers = append(m.tenantPurgers, %srepository.New%sTenantPurger(infra.DB))",
				m.Ident, r.Pascal))
		}},
		{"catalog/permissions.go", func(src string) (string, error) {
			perm := "Permission" + r.PluralPascal
			out, err := insertAtMarker("catalog/permissions.go", src, markerPermissions, fmt.Sprintf(
				"// %[1]sRead allows listing and reading %[2]s.\n%[1]sRead authz.Permission = %[3]q\n"+
					"// %[1]sWrite allows creating and updating %[2]s.\n%[1]sWrite authz.Permission = %[4]q\n"+
					"// %[1]sDelete allows deleting %[2]s.\n%[1]sDelete authz.Permission = %[5]q",
				perm, r.HumanPlural,
				m.Name+":"+r.Plural+":read", m.Name+":"+r.Plural+":write", m.Name+":"+r.Plural+":delete"))
			if err != nil {
				return "", err
			}
			return insertAtMarker("catalog/permissions.go", out, markerGrants, fmt.Sprintf(
				"{Permission: %[1]sRead},\n{Permission: %[1]sWrite},\n{Permission: %[1]sDelete},", perm))
		}},
		{"catalog/errors.go", func(src string) (string, error) {
			code := "Code" + r.Pascal + "NotFound"
			out, err := insertAtMarker("catalog/errors.go", src, markerCodes, fmt.Sprintf(
				"// %s: the %s does not exist in the request tenant.\n%s apperror.Code = %q",
				code, r.Human, code, notFoundCode(m, r)))
			if err != nil {
				return "", err
			}
			return insertAtMarker("catalog/errors.go", out, markerSpecs, fmt.Sprintf(
				"%s: {\n\tStatus:  http.StatusNotFound,\n\tTitle:   %q,\n\tDocPath: %q,\n},",
				code, r.Title+" not found", "/errors/"+m.Route+"-"+r.Route+"-not-found"))
		}},
		{"catalog/locales/en.json", localeEdit("catalog/locales/en.json", notFoundCode(m, r), r.Title+" not found")},
		{"catalog/locales/vi.json", localeEdit("catalog/locales/vi.json", notFoundCode(m, r), "Không tìm thấy "+r.Human)},
		{"README.md", func(src string) (string, error) {
			return insertAtMarker("README.md", src, markerReadme, fmt.Sprintf(
				"- Resource %#q: table %#q; routes `/api/v1/%s/%s[/:id]` (GET, POST, PATCH, DELETE); "+
					"permissions `%s:%s:{read,write,delete}`; error %#q.",
				r.Name, r.Table, m.Route, r.PluralRoute, m.Name, r.Plural, notFoundCode(m, r)))
		}},
	}
	for _, e := range edits {
		if err := g.editFile(p, dir+"/"+e.file, e.edit); err != nil {
			return err
		}
	}
	return nil
}

func notFoundCode(m moduleData, r resourceData) string {
	return m.Name + "." + r.Name + "_not_found"
}

func localeEdit(file, code, title string) func(string) (string, error) {
	return func(src string) (string, error) {
		out, err := mergeLocale(file, []byte(src), map[string]string{"errors." + code + ".title": title})
		return string(out), err
	}
}
