// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const I18N = require("../public/shell-assets/i18n.js");
const catalog = (language) => JSON.parse(fs.readFileSync(path.join(__dirname, "../public/shell-assets/i18n/", language + ".json"), "utf8"));

test("language resolution handles unsupported tags and inaccessible storage", () => {
  const storage = { getItem: () => "pt-PT" };
  assert.equal(I18N.resolve({search:"?lang=es-MX",storage,cookie:"elcano_language=en",languages:["en"]}), "es");
  assert.equal(I18N.resolve({storage,cookie:"elcano_language=es",languages:["en"]}), "pt");
  assert.equal(I18N.resolve({storage:{getItem(){throw Error("denied")}},cookie:"elcano_language=es",languages:["pt"]}), "es");
  assert.equal(I18N.resolve({search:"?lang=de",cookie:"elcano_language=no",languages:["fr", "pt-BR"]}), "pt");
  assert.equal(I18N.resolve({languages:["fr"]}), "en");
});

test("translations preserve interpolation values and do not translate user data", () => {
  const t = I18N.translator("es", catalog("es"));
  assert.equal(t("Loading {0}", "{0}<script>"), "Cargando {0}<script>");
  assert.equal(t("User authored text"), "User authored text");
  assert.equal(I18N.withLanguage("/admin?workspace=2#review", "pt"), "/admin?workspace=2&lang=pt#review");
  assert.equal(I18N.withLanguage("https://customer.example/page", "pt"), "https://customer.example/page");
});

test("browser initializes and switches through the URL when all storage access is denied", () => {
  let destination;
  const document = {getElementById:(id)=>({textContent:JSON.stringify(id==="pages-bootstrap"?{contentOrigin:"https://content.example"}:{language:"es",messages:catalog("es")})}),documentElement:{},addEventListener(){}};
  Object.defineProperty(document,"cookie",{get(){throw Error("cookie denied")},set(){throw Error("cookie denied")}});
  const window = {document,navigator:{languages:["en"]},location:{search:"?lang=es&workspace=2",href:"https://pages.example/admin?lang=es&workspace=2#review",protocol:"https:",origin:"https://pages.example",assign(value){destination=value},replace(){throw Error("unexpected reload")}}};
  Object.defineProperty(window,"localStorage",{get(){throw Error("storage denied")}});
  vm.runInNewContext(fs.readFileSync(path.join(__dirname,"../public/shell-assets/i18n.js"),"utf8"),{window,URL,URLSearchParams});
  assert.equal(window.PagesI18n.t("Client pages"),"Páginas de clientes");
  assert.equal(window.PagesI18n.ownedURL("https://content.example/raw/page?t=opaque"),"https://content.example/raw/page?t=opaque&lang=es");
  assert.equal(window.PagesI18n.ownedURL("https://customer.example/raw/page?t=opaque"),"https://customer.example/raw/page?t=opaque");
  window.PagesI18n.save("pt");
  assert.equal(destination,"/admin?lang=pt&workspace=2#review");
});

test("both catalogs cover every static browser message and preserve exact slots", () => {
  const es=catalog("es"), pt=catalog("pt");
  assert.deepEqual(Object.keys(es).sort(),Object.keys(pt).sort());
  const slots = (value) => [...value.matchAll(/\{\d+\}/g)].map((match)=>match[0]).sort();
  for(const [key,value] of Object.entries(es)) {
    assert.equal(typeof value,"string",key);assert.ok(value.trim(),key);
    assert.deepEqual(slots(value),slots(key),key+" Spanish slots");
    assert.deepEqual(slots(pt[key]),slots(key),key+" Portuguese slots");
  }
  const base=path.join(__dirname,"../public/shell-assets");
  let count=0;
  for(const file of fs.readdirSync(base).filter(file=>file.endsWith(".js"))) {
    const source=fs.readFileSync(path.join(base,file),"utf8");
    for(const match of source.matchAll(/\bt\(\s*("(?:[^"\\]|\\.)*")/g)) {
      const key=JSON.parse(match[1]);count++;
      assert.ok(Object.hasOwn(es,key),file+": missing "+key);
    }
  }
  assert.ok(count>750,"browser coverage unexpectedly shrank");
});
