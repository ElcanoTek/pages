// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { test, expect } = require("@playwright/test");

// The preflight verdict and the served CSP both come from the fixture server —
// the Go binary — so this pins the implementation that ships, and the browser
// suite needs nothing from the server's own dependencies. An own-origin URL is a
// function of the content origin the server reports.
let policy;
test.beforeAll(async ({ request }) => {
  policy = await (await request.get("/__fixture/content-policy")).json();
});

const PIXEL = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Zl1sAAAAASUVORK5CYII=";
const cases = [
  { name: "relative script", tag: "script", url: "/asset.js", directive: "script-src", allowed: true },
  { name: "absolute own-origin script", tag: "script", url: (origin) => `${origin}/asset.js`, directive: "script-src", allowed: true },
  { name: "protocol-relative own-origin script", tag: "script", url: (origin) => `//${new URL(origin).host}/asset.js`, directive: "script-src", allowed: true },
  { name: "protocol-relative external script", tag: "script", url: "//northwind.invalid/asset.js", directive: "script-src", allowed: false },
  { name: "data script", tag: "script", url: "data:text/javascript,globalThis.resourceLoaded=true", directive: "script-src", allowed: false },
  { name: "blob script", tag: "script", blob: "script", directive: "script-src", allowed: true },
  { name: "data image", tag: "img", url: `data:image/png;base64,${PIXEL}`, directive: "img-src", allowed: true },
  { name: "blob image", tag: "img", blob: "image", directive: "img-src", allowed: true },
  { name: "data stylesheet", tag: "link", url: "data:text/css,body{color:red}", directive: "style-src", allowed: false },
  { name: "own-origin media", tag: "video", url: "/asset.mp4", directive: "media-src", allowed: false },
  { name: "own-origin frame", tag: "iframe", url: "/asset.html", directive: "frame-src", allowed: false },
  { name: "blob font", tag: "font", blob: "font", directive: "font-src", allowed: false },
];

for (const resource of cases) {
  test(`preflight agrees with the served CSP for a ${resource.name}`, async ({ page, request }) => {
    const CONTENT_ORIGIN = policy.content_origin;
    // Every request is fulfilled locally, including synthetic external origins.
    await page.route("**/*", route => {
      if (new URL(route.request().url()).pathname === "/preflight-resource-fixture") {
        return route.fulfill({ contentType: "text/html", headers: policy.raw_headers, body: `<!doctype html><html><head><script>
          globalThis.violations=[]; globalThis.resourceLoaded=false;
          document.addEventListener('securitypolicyviolation',event=>violations.push(event.effectiveDirective));
        </script></head><body></body></html>` });
      }
      return route.fulfill({ contentType: "text/javascript", body: "globalThis.resourceLoaded=true;" });
    });
    await page.goto(`${CONTENT_ORIGIN}/preflight-resource-fixture`);
    const url = resource.blob ? await page.evaluate(({ kind, pixel }) => {
      const blob = kind === "image"
        ? new Blob([Uint8Array.from(atob(pixel), char => char.charCodeAt(0))], { type: "image/png" })
        : new Blob([kind === "script" ? "globalThis.resourceLoaded=true;" : "font fixture"], { type: kind === "script" ? "text/javascript" : "font/woff2" });
      return URL.createObjectURL(blob);
    }, { kind: resource.blob, pixel: PIXEL }) : typeof resource.url === "function" ? resource.url(CONTENT_ORIGIN) : resource.url;
    const html = resource.tag === "font"
      ? `<style>@font-face{font-family:Northwind;src:url('${url}')}</style>`
      : resource.tag === "link" ? `<link rel="stylesheet" href="${url}">` : `<${resource.tag} src="${url}"></${resource.tag}>`;
    const report = await (await request.post("/__fixture/preflight", { data: { html } })).json();
    const blocked = report.errors.some(finding => finding.code === "remote_subresource_blocked");
    expect(blocked).toBe(!resource.allowed);
    await page.evaluate(({ tag, url }) => {
      if (tag === "font") {
        const font = new FontFace("Northwind", `url('${url}')`);
        font.load().catch(() => {});
        return;
      }
      const node = document.createElement(tag);
      if (tag === "link") { node.rel = "stylesheet"; node.href = url; } else node.src = url;
      if (tag === "img") node.onload = () => { globalThis.resourceLoaded = true; };
      if (tag === "video") node.preload = "auto";
      document.body.appendChild(node);
      if (tag === "video") node.load();
    }, { tag: resource.tag, url });
    if (resource.allowed) {
      await expect.poll(() => page.evaluate(() => globalThis.resourceLoaded)).toBe(true);
      expect(await page.evaluate(() => globalThis.violations)).toEqual([]);
    } else {
      // Chromium reports the effective element directive even when it falls
      // back to the script-src/style-src source list in the served policy.
      const effective = resource.tag === "script" || resource.tag === "link"
        ? `${resource.directive}-elem` : resource.directive;
      await expect.poll(() => page.evaluate(() => globalThis.violations)).toContain(effective);
    }
  });
}
