// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const I18N = require("../public/shell-assets/i18n.js");
const Messages = require("../public/shell-assets/messages.js");
const catalog = (language) => JSON.parse(fs.readFileSync(path.join(__dirname, "../public/shell-assets/i18n", language + ".json"), "utf8"));
const localized = (language) => Messages.create({language, t: I18N.translator(language, catalog(language))}).message;

test("all known diagnostics have reviewed translations and exact numeric slots", () => {
  const slots = (text) => [...text.matchAll(/\{\d+\}/g)].map((match) => match[0]).sort();
  for (const language of ["es", "pt"]) {
    const values = catalog(language);
    for (const source of Messages.sources) {
      assert.ok(Object.hasOwn(values, source), language + ": " + source);
      assert.deepEqual(slots(values[source]), slots(source), language + ": " + source);
    }
  }
});

test("every allowlisted pattern retains each captured value in both translations", () => {
  const values = ["Northwind[0]", "Contoso[1]", "Fabrikam[2]", "Northwind[3]", "Contoso[4]", "Fabrikam[5]"];
  for (const language of ["es", "pt"]) {
    const t = I18N.translator(language, catalog(language));
    const message = localized(language);
    for (const source of Messages.sources) {
      assert.equal(message(I18N.interpolate(source, values)), t(source, ...values), language + ": " + source);
    }
  }
});

test("API errors translate owned wording while names and code values remain verbatim", () => {
  const es = localized("es"), pt = localized("pt");
  assert.equal(es("portal not found"), "Portal no encontrado");
  assert.equal(pt("portal not found"), "Portal não encontrado");
  const slug = '<img src=x onerror=alert(1)>/{0}/Northwind';
  assert.equal(es("page not found: " + slug), "Página no encontrada: " + slug);
  assert.equal(pt('sources[2].partition.by must be "date"'), 'sources[2].partition.by tem de ser "date"');
  assert.equal(es("can only approve a pending version (status=draft)"), "Solo se puede aprobar una versión pendiente (status=draft)");
  assert.equal(es("not the known error: portal not found"), "not the known error: portal not found");
  assert.equal(pt("Unknown upstream diagnostic: <script>{0}</script>"), "Unknown upstream diagnostic: <script>{0}</script>");
});

test("only declared nested diagnostics are translated, without interpreting user captures", () => {
  const es = localized("es"), pt = localized("pt");
  const source = 'The Pages-managed blocks do not satisfy their own contract: #pages-data must contain valid JSON';
  assert.equal(es(source), 'Los bloques gestionados por Pages no cumplen su propio contrato: #pages-data debe contener JSON válido');
  assert.equal(pt('refresh schedule: the minute field "*/*" has an invalid step (use a number from 1 to 59)'), 'Agendamento de atualização: O campo minute "*/*" tem um passo inválido (use um número entre 1 e 59)');
  assert.equal(es("page not found: portal not found"), "Página no encontrada: portal not found");
  const script = 'Inline <script> #1 does not parse: Customer source says "portal not found". Nothing in that block runs, so every function it declares is undefined and every control wired to one is dead.';
  assert.ok(es(script).includes('Customer source says "portal not found"'));
  const sandbox = 'window.open() does not work on the content host: its sandbox does not include "allow-popups". Returns null and no window opens.';
  assert.equal(pt(sandbox), 'window.open() não funciona no servidor de conteúdo: o seu ambiente isolado não inclui "allow-popups". Devolve null e não abre nenhuma janela.');
});

test("preflight summaries and complete plural variants avoid English grammar fragments", () => {
  const es = localized("es"), pt = localized("pt");
  assert.equal(es("Preflight found 1 error and 2 warnings. These are advisory — nothing was blocked."), "La comprobación previa encontró 1 error y 2 avisos. Son avisos; no se bloqueó nada.");
  assert.equal(pt("Preflight found 2 errors and 1 warning. These are advisory — nothing was blocked."), "A verificação prévia encontrou 2 erros e 1 aviso. São avisos; nada foi bloqueado.");
  const suffix = ' were omitted to keep this response bounded. A payload that narrows this many fields at once should be reconciled against its source before publishing, not triaged warning by warning.';
  assert.ok(es('1 further warning' + suffix).startsWith('Se omitió 1 aviso adicional'));
  assert.ok(pt('4 further warnings' + suffix).startsWith('Foram omitidos 4 avisos adicionais'));
  const pages = ' built from Northwind; pass force to retire it anyway. Those pages keep serving, but they lose their design provenance and can no longer be re-rendered from it. Call list_template_pages first to see which.';
  assert.ok(es('1 page was' + pages).startsWith('Se creó 1 página a partir de Northwind;'));
  assert.ok(pt('3 pages were' + pages).startsWith('Foram criadas 3 páginas a partir de Northwind;'));
  assert.equal(pt('must NOT have fewer than 2 characters'), 'NÃO pode ter menos de 2 carateres');
});

test("English and unknown input stay byte-identical, including unusual placeholders and whitespace", () => {
  const english = Messages.create({language:"en", t(){throw Error("English must not interpolate");}}).message;
  for (const text of [...Messages.sources, ' \npage not found: {0}<html>\r\n', 'invalid_request', null, undefined]) assert.equal(english(text), text);
  const text = "page not found: " + "a".repeat(65536);
  assert.equal(localized("es")(text), text);
});

test("the browser error presentation uses the current locale without modifying response data", () => {
  const window = {PagesI18n:{language:"pt",locale:"pt-PT",t:I18N.translator("pt",catalog("pt"))}};
  const context = {window,URL,URLSearchParams,console};
  for (const file of ["messages.js", "primitives.js"]) vm.runInNewContext(fs.readFileSync(path.join(__dirname,"../public/shell-assets",file),"utf8"),context);
  const body = {error:"portal not found",code:"portal_not_found",details:{slug:"portal not found"}};
  const before = JSON.stringify(body);
  assert.equal(window.PagesUI.describeError(404,body,""), "Portal não encontrado");
  assert.equal(JSON.stringify(body),before);
  assert.equal(window.PagesUI.describeError(502,null,"<html>upstream</html>"),"O pedido falhou (502)");
});

test("refresh health localizes owned dates and outcome captions without translating recorded detail", () => {
  const pt = Messages.create({language:"pt",t:I18N.translator("pt",catalog("pt"))});
  assert.equal(pt.outcome("source_not_updated"),"Fonte não atualizada");
  assert.equal(pt.outcome("source_unreachable"),"Fonte inacessível");
  assert.equal(pt.outcome("an_unknown_outcome"),"an_unknown_outcome");
  const raw = {health:"blocked",health_reason:"portal not found"};
  assert.equal(pt.healthReason(raw,{last_check_detail:"portal not found"}),"portal not found");
  assert.equal(pt.healthReason(raw),"portal not found");
  const reason = pt.healthReason({health:"blocked",health_reason:"The latest refresh run recorded source unreachable on Oct 20, with no detail."},{last_check_detail:null});
  assert.equal(reason,'A última atualização registou «Fonte inacessível» em 20/10, sem detalhes.');
  const overdue = pt.healthReason({health:"overdue",health_reason:"Expected a refresh at Oct 22 15:00 UTC; last activity Oct 20."});
  assert.ok(overdue.startsWith("Era esperada uma atualização em "));
  assert.ok(overdue.includes("15:00 UTC"));
  assert.ok(!overdue.includes("Oct"));
});

test("coverage gaps use complete localized sentences and preserve customer field names and entities", () => {
  const es = Messages.create({language:"es",t:I18N.translator("es",catalog("es"))});
  const gap = {path:"Northwind.rows",key_field:"dealId",fields:["revenue","portal not found"],fields_omitted:1,stopped_on:"2026-09-30",rows_through:"2026-10-26",entity_count:4,entities:['D-01','<b>{0}</b>','D-03'],message:"Original English gap"};
  const before = JSON.stringify(gap);
  const result = es.coverageGap(gap);
  assert.ok(result.startsWith("En Northwind.rows, los valores de "));
  for (const value of ['revenue','portal not found','1 valor adicional','4 entidades con clave dealId','"<b>{0}</b>"']) assert.ok(result.includes(value),value);
  assert.ok(!result.includes("2026-09-30"));
  assert.equal(JSON.stringify(gap),before);
  assert.ok(es.healthReason({health:"coverage_gap",health_reason:"Original English gap (+1 more gap)",coverage_gaps:[gap,gap]}).endsWith(" (+1 carencia más)"));
  assert.equal(Messages.create({language:"en",t:I18N.t}).coverageGap(gap),"Original English gap");
});
