package main

import (
	"context"
	"fmt"
	"strings"
)

func (g *generator) generateUseCase(ctx context.Context, kind, module, name string, withHTTP bool) error {
	p, data, err := g.planUseCase(kind, module, name, withHTTP)
	if err != nil {
		return err
	}
	next := []string{
		"Implement " + data.U.Name + "Handler.Handle in internal/modules/" + module + "/service/" + kind + "_" + data.U.Snake + ".go " +
			"(declare its dependencies as ports in service/ports.go) and replace the 501 test",
	}
	if withHTTP {
		next = append(next, "Fill the request/response DTOs and their mapping, then refresh the API spec: make openapi")
	}
	return g.execute(ctx, p, verifySteps(module, false), next)
}

func (g *generator) planUseCase(kind, module, name string, withHTTP bool) (*plan, templateData, error) {
	if err := g.requireGeneratedModule(module); err != nil {
		return nil, templateData{}, err
	}
	if err := validateUseCaseName(name); err != nil {
		return nil, templateData{}, err
	}
	data := templateData{M: newModuleData(g.modulePath, module), U: newUseCaseData(kind, name)}
	m, u := data.M, data.U
	dir := g.moduleDir(module)
	base := kind + "_" + u.Snake
	files := []struct{ template, path string }{
		{"usecase/service.go.tmpl", "service/" + base + ".go"},
		{"usecase/service_test.go.tmpl", "service/" + base + "_test.go"},
	}
	if withHTTP {
		files = append(files,
			struct{ template, path string }{"usecase/handler.go.tmpl", "handler/" + base + ".go"},
			struct{ template, path string }{"usecase/handler_test.go.tmpl", "handler/" + base + "_test.go"},
			struct{ template, path string }{"usecase/dto.go.tmpl", "dto/" + base + ".go"},
			struct{ template, path string }{"usecase/routes.go.tmpl", "routes_" + base + ".go"},
		)
	}
	p := &plan{repo: g.repo}
	for _, f := range files {
		content, err := render(f.template, data)
		if err != nil {
			return nil, templateData{}, err
		}
		p.create(dir+"/"+f.path, content)
	}

	route := "no HTTP route"
	if withHTTP {
		method := "GET"
		if u.IsCommand {
			method = "POST"
		}
		route = fmt.Sprintf("route `%s /api/v1/%s/%s`", method, m.Route, u.Route)
		if err := g.wireUseCase(p, data); err != nil {
			return nil, templateData{}, err
		}
	}
	if err := g.editFile(p, dir+"/README.md", func(src string) (string, error) {
		return insertAtMarker("README.md", src, markerReadme, fmt.Sprintf(
			"- %s %#q: `service/%s.go`; %s; permission `%s:%s`.",
			titleCase(kind), u.Name, base, route, m.Name, permissionVerb(u)))
	}); err != nil {
		return nil, templateData{}, err
	}
	return p, data, nil
}

// wireUseCase adds the HTTP adapter to the module's Handlers, routes, and
// Build at their gen: markers.
func (g *generator) wireUseCase(p *plan, data templateData) error {
	m, u := data.M, data.U
	dir := g.moduleDir(m.Name)
	if err := g.editFile(p, dir+"/module.go", func(src string) (string, error) {
		out, err := insertAtMarker("module.go", src, markerHandlers, fmt.Sprintf("%s *%shandler.%sHTTP", u.Name, m.Ident, u.Name))
		if err != nil {
			return "", err
		}
		return insertAtMarker("module.go", out, markerRoutes, fmt.Sprintf("m.register%sRoute(api)", u.Name))
	}); err != nil {
		return err
	}
	return g.editFile(p, dir+"/build.go", func(src string) (string, error) {
		return insertAtMarker("build.go", src, markerBuild, fmt.Sprintf(
			"%s: %shandler.New%sHTTP(%sservice.New%sHandler()),", u.Name, m.Ident, u.Name, m.Ident, u.Name))
	})
}

func permissionVerb(u useCaseData) string {
	if u.IsCommand {
		return "write"
	}
	return "read"
}

func titleCase(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
