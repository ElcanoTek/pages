// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations } = require("./helpers");

test.beforeEach(async ({request})=>resetFixture(request));

for(const language of ["es","pt"]) {
  const pages="Páginas de clientes", templates=language==="es"?"Biblioteca de plantillas":"Biblioteca de modelos", portals=language==="es"?"Portales de socios":"Portais de parceiros";
  test(language+" localizes all dashboard screens while preserving stored titles", async({page})=>{
    const errors=[];page.on("pageerror",error=>errors.push(error.message));
    await page.goto("/admin?lang="+language);
    await expect(page.getByRole("heading",{level:1,name:pages})).toBeVisible();
    await expect(page.locator("html")).toHaveAttribute("lang",language);
    await expect(page.getByRole("link",{name:"Client 01 operations dashboard",exact:true})).toBeVisible();
    for (const width of [1280, 1201, 900, 390, 320]) {
      await page.setViewportSize({width,height:800});
      await expectNoHorizontalOverflow(page);
      if (width >= 1201) {
        const badge = await page.locator(".refresh-health .badge").first().evaluate(node => {
          const style = getComputedStyle(node);
          return {height:node.getBoundingClientRect().height, twoLines:2 * parseFloat(style.lineHeight) + parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) + 2};
        });
        expect(badge.height).toBeLessThanOrEqual(badge.twoLines + 1);
      }
      const cells = await page.locator(".operation-table--index tbody tr").first().locator("td").evaluateAll(nodes => nodes.map(node => {
        const status = node.querySelector(".status");
        return {key:node.dataset.column, area:getComputedStyle(node).gridArea, right:node.getBoundingClientRect().right, statusRight:status?.getBoundingClientRect().right};
      }));
      expect(cells).toHaveLength(6);
      for (const cell of cells) {
        if (width <= 1200) expect(cell.area.split(" /")[0]).toBe(cell.key);
        if (cell.statusRight) expect(cell.statusRight).toBeLessThanOrEqual(cell.right);
      }
    }
    await page.setViewportSize({width:1280,height:800});
    await page.getByRole("link",{name:"Client 01 operations dashboard",exact:true}).click();
    await expect(page).toHaveURL(new RegExp("lang="+language));
    await expect(page.getByRole("heading",{level:1,name:"Client 01 operations dashboard"})).toBeVisible();
    await expect(page.getByRole("heading",{name:language==="es"?"Configuración":"Definições",exact:true})).toBeVisible();
    await page.goto("/admin/long/client/q2-report?lang="+language);
    await expect(page.locator("#preview-open-full")).toHaveAttribute("href",new RegExp("lang="+language));
    await page.goto("/admin/templates?lang="+language);
    await expect(page.getByRole("heading",{level:1,name:templates})).toBeVisible();
    await expect(page.getByRole("columnheader",{name:"Páginas",exact:true})).toBeVisible();
    await expect(page.getByRole("button",{name:language==="es"?"Añadir una plantilla":"Adicionar um modelo",exact:true})).toBeVisible();
    await page.goto("/admin/portals?lang="+language);
    await expect(page.getByRole("heading",{level:1,name:portals})).toBeVisible();
    await expect(page.getByRole("columnheader",{name:"Páginas",exact:true})).toBeVisible();
    await expectNoHorizontalOverflow(page);
    await expectNoSeriousAxeViolations(page,language+" portal admin");
    for (const width of [320, 390]) {
      await page.setViewportSize({width,height:800});
      await expectNoHorizontalOverflow(page);
    }
    const light=language==="es"?"Cambiar al modo claro":"Mudar para o modo claro";
    await page.getByRole("button",{name:light,exact:true}).click();
    await expect(page.getByRole("button",{name:language==="es"?"Cambiar al modo oscuro":"Mudar para o modo escuro",exact:true})).toBeVisible();
    expect(errors).toEqual([]);
  });
}

test("language changes preserve URL state and persist a host-only preference",async({page,context})=>{
  await page.goto("/admin?workspace=2&lang=es#workspace-index");
  await expect(page.getByRole("heading",{level:1,name:"Páginas de clientes"})).toBeVisible();
  await page.locator("#pages-language").selectOption("pt");
  await expect(page.locator("html")).toHaveAttribute("lang","pt");
  await expect(page).toHaveURL(/workspace=2&lang=pt#workspace-index$/);
  expect(await page.evaluate(()=>localStorage.getItem("elcano.language"))).toBe("pt");
  const cookie=(await context.cookies()).find(cookie=>cookie.name==="elcano_language");
  expect(cookie).toMatchObject({value:"pt",domain:"127.0.0.1",path:"/",sameSite:"Lax"});
  await page.goto("/admin/templates");
  await expect(page.getByRole("heading",{level:1,name:"Biblioteca de modelos"})).toBeVisible();
});

test("denied storage still permits language changes and section navigation",async({page})=>{
  await page.addInitScript(()=>{
    Object.defineProperty(window,"localStorage",{get(){throw new DOMException("denied","SecurityError")}});
    Object.defineProperty(document,"cookie",{get(){throw new DOMException("denied","SecurityError")},set(){throw new DOMException("denied","SecurityError")}});
  });
  await page.goto("/admin?lang=es&workspace=2#workspace-index");
  await expect(page.getByRole("heading",{level:1,name:"Páginas de clientes"})).toBeVisible();
  // The href itself carries the language, including for opening a new tab.
  await expect(page.locator('.operation-table tbody a[href^="/admin/"]').first()).toHaveAttribute("href",/lang=es/);
  await page.locator("#pages-language").selectOption("pt");
  await expect(page).toHaveURL(/lang=pt&workspace=2#workspace-index$/);
  await page.getByRole("link",{name:"Biblioteca de modelos",exact:true}).click();
  await expect(page).toHaveURL(/\/admin\/templates\?lang=pt$/);
  await expect(page.getByRole("heading",{level:1,name:"Biblioteca de modelos"})).toBeVisible();
  await page.route("**/logout*", route => route.fulfill({status:200, contentType:"text/html", body:"<!doctype html><title>Signed out</title>"}));
  await page.locator("button.header-signout").click();
  await page.getByRole("dialog").getByRole("button",{name:"Terminar sessão",exact:true}).click();
  await expect(page).toHaveURL(/\/logout\?lang=pt$/);
});

test("browser language supplies the initial choice with English fallback",async({browser})=>{
  for(const [locale,expected,title] of [["es-MX","es","Páginas de clientes"],["pt-BR","pt","Páginas de clientes"],["de-DE","en","Client pages"]]){
    const context=await browser.newContext({locale});const page=await context.newPage();
    await page.goto(process.env.PLAYWRIGHT_FIXTURE_PORT?`http://127.0.0.1:${process.env.PLAYWRIGHT_FIXTURE_PORT}/admin`:"http://127.0.0.1:3210/admin");
    await expect(page.getByRole("heading",{level:1,name:title})).toBeVisible();
    await expect(page.locator("html")).toHaveAttribute("lang",expected);await context.close();
  }
});

test("scriptless gates retain localized forms and customer names",async({browser,baseURL})=>{
  const context=await browser.newContext({javaScriptEnabled:false,baseURL});const page=await context.newPage();
  await page.goto("/gate/password?lang=pt");
  await expect(page.getByRole("heading",{name:"Esta página está protegida"})).toBeVisible();
  await expect(page.getByRole("button",{name:"Ver página"})).toBeVisible();
  await expect(page.locator("form")).toHaveAttribute("action","/gate/password?lang=pt");
  await expect(page.locator("script")).toHaveCount(0);
  await page.getByRole("link",{name:"Español"}).click();
  await expect(page.getByLabel("Contraseña de la página")).toBeVisible();
  await page.goto("/gate/portal-open?lang=es");
  await expect(page.getByRole("heading",{name:"Northwind Media Group"})).toBeVisible();
  await expect(page.getByText("Empieza aquí",{exact:true})).toBeVisible();
  await expect(page.locator(".portal-list a").first()).toHaveAttribute("href",/lang=es/);
  await page.goto("/admin?lang=pt");
  await page.route("**/logout*", route => route.fulfill({status:200, contentType:"text/html", body:"<!doctype html><title>Signed out</title>"}));
  await page.locator("button.header-signout").click();
  await expect(page).toHaveURL(/\/logout\?lang=pt$/);
  await context.close();
});

test("the injected Page menu translates without modifying customer content",async({page})=>{
  await page.goto("/dashboard/raw-with-portal?lang=pt");
  await expect(page.getByRole("heading",{name:"Harbor Sun · flight to date"})).toBeVisible();
  await expect(page.locator(".pgnav-host summary")).toHaveText("Página▾");
  await page.locator(".pgnav-host summary").click();
  await expect(page.locator(".pgnav-host .pgnav__home")).toHaveText("Comece aqui");
  await expect(page.locator(".pgnav-host a").first()).toHaveAttribute("href",/lang=pt/);
});
