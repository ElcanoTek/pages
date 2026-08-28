// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The end of the chain the partner-portal feature exists for: an injected
// #pages-nav block becoming a Page menu a partner can click. Rendered through the
// real render path from the real shipped template, so a design change that breaks
// the control fails here instead of in front of a partner.

const { test, expect } = require("@playwright/test");
const { expectNoHorizontalOverflow } = require("./helpers");

test("a portal-authorised dashboard grows a Page menu of real links", async ({ page }) => {
  await page.goto("/dashboard/with-switcher");
  const control = page.locator("details.pagenav");
  await expect(control).toBeVisible();
  await expect(control.locator("summary")).toContainText("Page");

  // Closed until asked: a native <details>, so the disclosure needs no script and
  // survives a scripting failure elsewhere on the page.
  await expect(control).not.toHaveAttribute("open", /.*/);
  await control.locator("summary").click();
  // "Dashboard pages" was our filing label; the head now names the partner's
  // portal, and links to its index.
  await expect(control.getByRole("link", { name: "Northwind Media Group" })).toBeVisible();

  // Not `a` alone: the head is a link to the portal index now, so the page
  // links are the ones that are not the head.
  const links = control.locator("nav.pagenav__menu a:not(.pagenav__label)");
  await expect(links).toHaveCount(4);
  // Real anchors with the ready-made url from the payload — never a scripted
  // window.open, which the sandbox silently swallows (allow-popups is withheld).
  await expect(links.nth(0)).toHaveAttribute("href", "/live/nwm-client-overview");
  await expect(links.nth(1)).toHaveAttribute("href", "/live/nwm/contoso-allergex");
  await expect(links.nth(1)).toHaveAttribute("aria-current", "page");
  await expect(links.nth(0)).not.toHaveAttribute("aria-current", /.*/);
  await expect(links.nth(1)).toContainText("Contoso — Allergex always-on");

  // A sibling's title is chosen by whoever owns that page. escapedJson keeps it
  // from ending the JSON block; textContent is what keeps it from becoming markup
  // inside this client's dashboard.
  await expect(links.nth(3)).toHaveText("Zeta <img src=x onerror=alert(1)> Q4");
  await expect(control.locator("img")).toHaveCount(0);
});

test("the menu closes on Escape and on a click outside", async ({ page }) => {
  await page.goto("/dashboard/with-switcher");
  const control = page.locator("details.pagenav");
  await control.locator("summary").click();
  await expect(control).toHaveAttribute("open", /.*/);
  await page.keyboard.press("Escape");
  await expect(control).not.toHaveAttribute("open", /.*/);
  await expect(control.locator("summary")).toBeFocused();

  await control.locator("summary").click();
  await expect(control).toHaveAttribute("open", /.*/);
  await page.locator("h1, .brandrow").first().click({ position: { x: 2, y: 2 } });
  await expect(control).not.toHaveAttribute("open", /.*/);
});

test("the switcher is absent, silently, when there is no payload to show", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));

  await page.goto("/dashboard/no-switcher");
  await expect(page.locator("details.pagenav")).toHaveCount(0);
  // The block is optional by design, so its absence must not throw — an unguarded
  // JSON.parse here is what takes the rest of the dashboard's script down with it.
  expect(errors).toEqual([]);
  // The dashboard itself still rendered.
  await expect(page.locator(".brandrow")).toBeVisible();

  // One entry is not a switcher: the only place to go is where you already are.
  await page.goto("/dashboard/one-page");
  await expect(page.locator("details.pagenav")).toHaveCount(0);
  expect(errors).toEqual([]);
});

test("the Page menu is keyboard reachable and does not widen the dashboard", async ({ page }) => {
  const widthAt = async (url, width) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(url);
    return page.evaluate(() => document.body.scrollWidth);
  };
  // This template is a dense table dashboard that already exceeds a phone
  // viewport on its own — 424px at 390 — so the honest assertion is not an
  // absolute bound it never met, but that adding the switcher costs nothing. The
  // menu is absolutely positioned and width-capped for exactly this reason.
  for (const width of [390, 768, 1440]) {
    const plain = await widthAt("/dashboard/no-switcher", width);
    const withMenu = await widthAt("/dashboard/with-switcher", width);
    expect(withMenu, `switcher must not widen the page at ${width}px`).toBe(plain);
  }

  await page.setViewportSize({ width: 390, height: 900 });
  await page.goto("/dashboard/with-switcher");
  const control = page.locator("details.pagenav");
  await control.locator("summary").focus();
  await page.keyboard.press("Enter");
  await expect(control).toHaveAttribute("open", /.*/);
  // Open, the menu must stay inside the viewport rather than pushing the page out.
  const openWidth = await page.evaluate(() => document.body.scrollWidth);
  const closedWidth = await widthAt("/dashboard/with-switcher", 390);
  expect(openWidth, "an open menu must not widen the page either").toBe(closedWidth);
});

// ── the built-in control: what makes this work on dashboards that predate it ──
// Every page that existed before the switcher shipped has no code to read the
// payload. Without a control supplied by Pages, a partner reaching one of those
// has no way onward except navigating back to the portal index — which is exactly
// the complaint this closes.

test("a legacy dashboard with no switcher code still gets a working Page menu", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("/dashboard/legacy-with-portal");

  const control = page.locator("details.pgnav");
  await expect(control).toBeVisible();
  await expect(control.locator("summary")).toContainText("Page");
  await control.locator("summary").click();

  const links = control.locator("a.pgnav__item");
  await expect(links).toHaveCount(3); // 4 entries, one is the current page and unlinked
  await expect(links.first()).toHaveAttribute("href", "/live/nwm-client-overview");
  await expect(control.locator(".pgnav__item--current")).toHaveAttribute("aria-current", "page");
  await expect(control.getByText("Northwind Media Group")).toBeVisible();

  // The hostile sibling title arrives as text, and builds no element.
  await expect(links.nth(2)).toHaveText("Zeta <img src=x onerror=alert(1)> Q4");
  await expect(control.locator("img")).toHaveCount(0);
  expect(errors).toEqual([]);

  // The dashboard underneath is untouched.
  await expect(page.getByRole("heading", { level: 1, name: "Q3 delivery" })).toBeVisible();
  await expect(page.getByRole("cell", { name: "$412,300" })).toBeVisible();
});

test("the built-in control never moves the dashboard it lands on", async ({ page }) => {
  // It is position:fixed precisely so it cannot reflow somebody else's design.
  const widthAt = async (url, width) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(url);
    return page.evaluate(() => ({ w: document.body.scrollWidth, h: document.body.scrollHeight }));
  };
  for (const width of [390, 768, 1440]) {
    const plain = await widthAt("/dashboard/legacy-no-portal", width);
    const withMenu = await widthAt("/dashboard/legacy-with-portal", width);
    expect(withMenu, `no reflow at ${width}px`).toEqual(plain);
  }
});

test("no portal, no control — and a design with its own menu is not given a second", async ({ page }) => {
  await page.goto("/dashboard/legacy-no-portal");
  await expect(page.locator("details.pgnav")).toHaveCount(0);
  // The shipped template reads the block itself, so it keeps its own control and
  // must not also receive Pages'.
  await page.goto("/dashboard/with-switcher");
  await expect(page.locator("details.pagenav")).toHaveCount(1);
  await expect(page.locator("details.pgnav")).toHaveCount(0);
});

test("the built-in control is keyboard operable and closes on Escape", async ({ page }) => {
  await page.goto("/dashboard/legacy-with-portal");
  const control = page.locator("details.pgnav");
  await control.locator("summary").focus();
  await page.keyboard.press("Enter");
  await expect(control).toHaveAttribute("open", /.*/);
  await page.keyboard.press("Escape");
  await expect(control).not.toHaveAttribute("open", /.*/);
  await expect(control.locator("summary")).toBeFocused();
});

test("the built-in control stays away when there is nowhere to go", async ({ page }) => {
  // A portal holding one dashboard gives a partner nothing to switch to, so a
  // control would be a menu whose only entry is the page you are already on.
  await page.goto("/dashboard/legacy-one-page");
  await expect(page.locator("details.pgnav")).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 1, name: "Q3 delivery" })).toBeVisible();
});

test("the built-in control is idempotent if DOMContentLoaded is dispatched again", async ({ page }) => {
  // Its listener stays registered, and polyfills and some frameworks re-dispatch
  // this event — so building twice has to be impossible, not merely unlikely.
  await page.goto("/dashboard/legacy-with-portal");
  await expect(page.locator("details.pgnav")).toHaveCount(1);
  await page.evaluate(() => document.dispatchEvent(new Event("DOMContentLoaded")));
  await page.evaluate(() => document.dispatchEvent(new Event("DOMContentLoaded")));
  await expect(page.locator("details.pgnav")).toHaveCount(1);
});

// ── raw pages: the majority of the live fleet ────────────────────────────────

test("a raw dashboard gets the Page menu without being restyled", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("/dashboard/raw-with-portal");

  // The menu works.
  const control = page.locator("details.pgnav");
  await expect(control).toBeVisible();
  await control.locator("summary").click();
  await expect(control.locator("a.pgnav__item")).toHaveCount(3);
  await expect(control.locator("a.pgnav__item").first()).toHaveAttribute("href", "/live/nwm-client-overview");
  expect(errors).toEqual([]);

  // And the design is untouched — this is the promise `raw` actually makes.
  const styling = await page.evaluate(() => {
    const h1 = document.querySelector("h1");
    const body = document.body;
    return {
      headingFont: getComputedStyle(h1).fontFamily,
      headingColor: getComputedStyle(h1).color,
      bodyBg: getComputedStyle(body).backgroundColor,
      bodyFont: getComputedStyle(body).fontFamily,
      flagLinks: document.querySelectorAll('link[href*="design-tokens"],link[href*="/fonts/fonts.css"]').length,
      themeController: document.querySelectorAll('script[src*="theme-controller"]').length,
    };
  });
  expect(styling.flagLinks, "no Flag stylesheets on a raw page").toBe(0);
  expect(styling.themeController, "no theme controller on a raw page").toBe(0);
  expect(styling.headingFont).toContain("Georgia");
  expect(styling.headingColor).toBe("rgb(122, 31, 61)");
  expect(styling.bodyBg).toBe("rgb(255, 250, 245)");

  // Byte-level: identical computed styling to the same page with no portal.
  await page.goto("/dashboard/raw-no-portal");
  const plain = await page.evaluate(() => {
    const h1 = document.querySelector("h1");
    return {
      headingFont: getComputedStyle(h1).fontFamily,
      headingColor: getComputedStyle(h1).color,
      bodyBg: getComputedStyle(document.body).backgroundColor,
      bodyFont: getComputedStyle(document.body).fontFamily,
    };
  });
  expect({ ...styling, flagLinks: undefined, themeController: undefined }).toEqual({
    ...plain,
    flagLinks: undefined,
    themeController: undefined,
  });
  await expect(page.locator("details.pgnav")).toHaveCount(0);
});

test("the switcher does not reflow a raw dashboard either", async ({ page }) => {
  const sizeAt = async (url, width) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(url);
    return page.evaluate(() => ({ w: document.body.scrollWidth, h: document.body.scrollHeight }));
  };
  for (const width of [390, 768, 1440]) {
    expect(await sizeAt("/dashboard/raw-with-portal", width), `no reflow at ${width}px`)
      .toEqual(await sizeAt("/dashboard/raw-no-portal", width));
  }
});

test("the menu can reach the portal index the partner's link points at", async ({ page }) => {
  // The BUILT-IN control, which is what these two fix — the shipped template
  // renders its own menu and is covered above.
  await page.goto("/dashboard/legacy-with-portal");
  const menu = page.locator("details.pgnav");
  await menu.locator("summary").click();

  // The portal index is the one surface that always works for a partner. The menu
  // listed siblings and offered no way back to it, even though the payload carried
  // the portal's slug all along.
  const head = menu.getByRole("link", { name: /Northwind Media Group/ });
  await expect(head).toHaveAttribute("href", "/live/portal/nwm");

  // And it says which page that index puts first.
  const home = menu.locator(".pgnav__item", { hasText: "Portfolio overview" });
  await expect(home.locator(".pgnav__home")).toHaveText("Start here");
});

test("a truncated menu offers the portal rather than describing it", async ({ page }) => {
  await page.goto("/dashboard/legacy-truncated");
  const menu = page.locator("details.pgnav");
  await menu.locator("summary").click();
  // "More are available from your portal link" was dead text with the URL one
  // string away in the same payload.
  const more = menu.getByRole("link", { name: "See all in your portal" });
  await expect(more).toHaveAttribute("href", "/live/portal/nwm");
});

test("the control is sealed off from the dashboard it lands in", async ({ page }) => {
  await page.goto("/dashboard/raw-with-portal");
  const sealed = await page.evaluate(() => {
    const host = document.querySelector(".pgnav-host");
    if (!host) return { host: false };
    return {
      host: true,
      // A shadow root is what makes leakage impossible in BOTH directions: the
      // dashboard's own `a:visited` cannot out-specify .pgnav__item, and a design's
      // `*:focus{outline:none}` cannot take the focus ring away.
      shadow: Boolean(host.shadowRoot),
      insideShadow: Boolean(host.shadowRoot && host.shadowRoot.querySelector("details.pgnav")),
      // Nothing of ours is in the page's own tree to be styled by it.
      leakedIntoPage: document.body.querySelector(":scope > details.pgnav") !== null,
    };
  });
  expect(sealed).toEqual({ host: true, shadow: true, insideShadow: true, leakedIntoPage: false });
});

test("the pill is legible on a dark report and on a light one", async ({ page }) => {
  const pill = async (path) => {
    await page.goto(path);
    await expect(page.locator("details.pgnav")).toBeVisible();
    return page.evaluate(() => {
      const s = document.querySelector(".pgnav-host").shadowRoot.querySelector("summary");
      const own = getComputedStyle(s);
      const rgb = (v) => (v.match(/\d+/g) || []).slice(0, 3).map(Number);
      const lum = (v) => { const [r, g, b] = rgb(v); return (0.2126 * r + 0.7152 * g + 0.0722 * b) / 255; };
      return { bg: lum(own.backgroundColor), fg: lum(own.color) };
    });
  };

  // A raw dashboard is served with no Flag tokens, so the old sheet fired every
  // fallback at once: a white pill parked on a dark bespoke report.
  const dark = await pill("/dashboard/raw-dark");
  expect(dark.bg, "a dark report must not get a white pill").toBeLessThan(0.45);
  expect(dark.fg, "and its text must be light").toBeGreaterThan(0.5);

  const light = await pill("/dashboard/raw-with-portal");
  expect(light.bg, "a light report keeps a light pill").toBeGreaterThan(0.6);
  expect(light.fg).toBeLessThan(0.5);
});

test("on a phone the control leaves the corner the dashboard's own header is in", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 780 });
  await page.goto("/dashboard/raw-with-portal");
  await expect(page.locator("details.pgnav")).toBeVisible();
  const placed = await page.evaluate(() => {
    const box = document.querySelector(".pgnav-host").getBoundingClientRect();
    return { top: box.top, bottom: box.bottom, viewport: window.innerHeight };
  });
  // It used to sit over the header badges. Below 640px it goes to the bottom,
  // which is thumb reach and where designs put least.
  expect(placed.top).toBeGreaterThan(placed.viewport / 2);
  expect(placed.viewport - placed.bottom).toBeLessThan(40);
});

test("a design can relocate the control instead of fighting it", async ({ page }) => {
  await page.goto("/dashboard/raw-relocated");
  await expect(page.locator("details.pgnav")).toBeVisible();
  const box = await page.locator(".pgnav-host").evaluate((node) => node.getBoundingClientRect().top);
  // --pages-nav-top / --pages-nav-right are the sanctioned way; the control is in
  // a shadow root precisely so a design cannot reach it any other way.
  expect(Math.round(box)).toBe(64);
});

// #174 — the shipped NWM template, rendered through the real path. Everything
// above tests the control Pages injects; these two test the dashboard a partner
// actually reads on a phone.

for (const width of [320, 390, 768]) {
  test(`the NWM template fits the viewport at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/dashboard/with-switcher");
    // The header put the title and the "Data through / Flight / Sources last
    // refreshed" stamp in one non-wrapping row with a 230px minimum on the stamp.
    // At 390px that put the stamp's right edge at 424px and the whole dashboard
    // panned sideways under the reader's thumb.
    await expectNoHorizontalOverflow(page);
    const stamp = page.locator(".stampblock");
    const box = await stamp.boundingBox();
    expect(box.x + box.width, "the stamp block is inside the viewport").toBeLessThanOrEqual(width + 1);
  });
}

test("the NWM template loads without complaining to the console", async ({ page }) => {
  const problems = [];
  page.on("console", (message) => {
    if (message.type() === "error") problems.push(message.text());
  });
  page.on("pageerror", (error) => problems.push(`uncaught: ${error.message}`));
  await page.goto("/dashboard/with-switcher");
  await page.waitForLoadState("networkidle");
  // Six of these on every load: `d` attributes in the inline logo ended ".Z",
  // where the stray dot sits exactly where a path command has to. The parser
  // stopped at the error and drew the valid prefix, so the logo always looked
  // right — the cost was six red lines in the console of a document that agents
  // and operators are frequently debugging in, drowning the real ones.
  expect(problems, problems.join("\n")).toHaveLength(0);
});

test("a hostile theme cannot escape the style element in a real browser", async ({ page }) => {
  // #189. lib/render.js spliced a theme's override_css into a <style> verbatim.
  // A `</style>` anywhere in it — inside a CSS string is enough — ended the
  // element, and everything after it was parsed as markup in the CLIENT's
  // document. The unit test pins the escaping; only a browser proves the parser
  // agrees.
  await page.goto("/dashboard/hostile-theme");
  await expect(page.locator("h1")).toBeVisible();
  const state = await page.evaluate(() => ({
    pwned: typeof window.__pwned !== "undefined",
    // The theme is still applied: the escaping has to be meaning-preserving, or
    // the fix is a different bug.
    applied: getComputedStyle(document.documentElement).getPropertyValue("--pwn").trim(),
    // No element was created from the payload, anywhere in the document.
    scripts: [...document.querySelectorAll("script")].filter((s) => /__pwned/.test(s.textContent)).length,
  }));
  expect(state.pwned, "the theme executed script in the client's document").toBe(false);
  expect(state.applied).toBe("1");
  expect(state.scripts).toBe(0);
});
