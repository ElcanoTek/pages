# templates/

The page templates this deployment ships. `pages template sync` registers every
one of them, and `pages update` runs that automatically once the new release passes its readiness check —
so shipping a design fix is a git push and a deploy.

The `pages` binary embeds this directory, so a bare `pages template sync`
registers exactly the tree its release was built from.

Full contract: **[../docs/TEMPLATES.md](../docs/TEMPLATES.md)**.

## Layout

```
templates/
  <template-name>/
    template.html     # required — a complete managed page (four blocks)
    template.json     # optional — { "title", "description", "note", "name" }
```

The directory name is the template name (`a-z 0-9 - _`, no slashes) unless
`template.json` sets `name`. That keeps `sync` predictable: what is in the tree is
what gets registered.

## What a template.html must be

A **complete, self-validating page**, not a fragment. It carries both managed
pairs, its `#pages-config` holds a real reference config, and its `#pages-data`
holds the empty-state envelope — each satisfying its own schema. Registration
validates that, so a broken design is rejected here rather than inherited by every
page built from it.

```html
<script type="application/schema+json" id="pages-config-schema">{ … }</script>
<script type="application/json"        id="pages-config">{ …reference config… }</script>
<script type="application/schema+json" id="pages-data-schema">{ … }</script>
<script type="application/json"        id="pages-data">{"contract_version":1,…,"data":{ …empty state… }}</script>
<script>
  const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
  const DATA   = JSON.parse(document.getElementById('pages-data').textContent).data;
  /* render layer */
</script>
```

Both schemas must be self-contained **JSON Schema 2020-12** with an object root.
Local `$ref`s work; external ones are rejected.

## What belongs here

Every install registers every template in this directory, so a design here must
be reusable and carry invented reference data only, never a client's identity. A
design that belongs to one client is registered on that client's deployment.
Templates can come from a bundle or any other checkout: `pages template register
<path>` takes any file, and an agent can register one over MCP with
`create_upload_ticket{template}` → `register_template_upload`. This directory is
the set that ships *with* the deployment.

## Editing one

A registered revision is immutable. Editing `template.html` and re-syncing records
revision N+1 and moves the template's `current` pointer — and changes **nothing
that is already deployed**, because every page stays pinned to the revision it was
built from. Moving a live page onto a new revision is a separate, explicit step:

```
pages template list                          # who is on what
mcp: list_template_pages                     # which pages are behind
mcp: rerender_page_from_template  slug=…     # ONE page, publish=false by default
```

Re-syncing unchanged bytes is a no-op — registration dedupes on the content hash
against the newest revision.
