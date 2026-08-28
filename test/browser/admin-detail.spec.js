// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations } = require("./helpers");

const DETAIL = "/admin/long/client/q2-report";

test.beforeEach(async ({ request }) => resetFixture(request));

async function openDetail(page) {
  await page.goto(DETAIL);
  await expect(page.getByRole("heading", { level: 1, name: /North America Programmatic/ })).toBeVisible();
}

test("defaults to newest pending version, auto-previews it, and shows only valid actions", async ({ page }) => {
  await openDetail(page);
  await expect(page.getByRole("button", { name: "Needs review (2)" })).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator('.version-option[aria-current="true"]')).toContainText("Version 6");
  await expect(page.locator("#preview-status")).toHaveText("Previewing version 6");
  await expect(page.frameLocator("#preview-frame").getByRole("heading", { name: "Fixture preview #106" })).toBeVisible();
  const actions = page.locator(".preview-toolbar .version-actions");
  await expect(actions.getByRole("button", { name: "Reload preview" })).toBeVisible();
  // The page is approval-gated, so approving IS publishing — the button says so.
  await expect(actions.getByRole("button", { name: "Approve & publish", exact: true })).toBeVisible();
  await expect(actions.getByRole("button", { name: "Reject" })).toBeVisible();
  await expect(actions.getByRole("button", { name: "Publish", exact: true })).toHaveCount(0);
  await expect(page.getByText("Refresh the revenue totals", { exact: false })).toBeVisible();
  await expectNoSeriousAxeViolations(page, "detail default review");
});

test("publish, reject, approve, and rollback retain optimistic-concurrency payloads", async ({ page, request }) => {
  await openDetail(page);
  await page.getByRole("button", { name: "All versions (6)" }).click();
  await page.locator(".version-option").filter({ hasText: "Version 4" }).click();
  let actions = page.locator(".preview-toolbar .version-actions");
  await expect(actions.getByRole("button", { name: "Publish", exact: true })).toBeVisible();
  await expect(actions.getByRole("button", { name: /Approve/ })).toHaveCount(0);
  await actions.getByRole("button", { name: "Publish", exact: true }).click();
  // Publishing a draft is client-visible, so it is confirmed like every other
  // pointer move, and the confirmation names both versions.
  const publishDialog = page.getByRole("dialog", { name: "Publish version 4?" });
  await expect(publishDialog).toContainText("replaces live version 2");
  await expect(publishDialog).toContainText("version 2 stays available to roll back to");
  await publishDialog.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByText("Live version 4", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: /All versions/ }).click();
  await page.locator(".version-option").filter({ hasText: "Version 1" }).click();
  actions = page.locator(".preview-toolbar .version-actions");
  await actions.getByRole("button", { name: "Roll back" }).click();
  const rollback = page.getByRole("dialog", { name: "Roll back to version 1?" });
  await rollback.getByRole("button", { name: "Roll back" }).click();
  await expect(page.getByText("Live version 1", { exact: true })).toBeVisible();

  let events = (await (await request.get("/__fixture/events")).json()).events;
  const publishEvent = events.find((event) => event.path.endsWith("/publish"));
  const rollbackEvent = events.find((event) => event.path.endsWith("/rollback"));
  expect(publishEvent.body).toEqual({ version_id: 104, expected_version: 102 });
  expect(rollbackEvent.body).toEqual({ version_id: 101, expected_version: 104 });

  await resetFixture(request);
  await page.reload();
  await page.getByRole("button", { name: "Reject" }).click();
  await page.getByRole("dialog", { name: "Reject version 6?" }).getByRole("button", { name: "Reject version" }).click();
  await expect(page.locator('.version-option[aria-current="true"]')).toContainText("Version 5");
  await page.getByRole("button", { name: "Approve & publish", exact: true }).click();
  await page.getByRole("dialog", { name: "Approve and publish version 5?" })
    .getByRole("button", { name: "Approve & publish", exact: true }).click();
  await expect(page.getByText("Live version 5", { exact: true })).toBeVisible();
  events = (await (await request.get("/__fixture/events")).json()).events;
  expect(events.find((event) => event.path.endsWith("/versions/105/approve")).body).toEqual({ expected_version: 102 });
});

test("client access supports URL copy, password generation/update, and confirmed clearing", async ({ page, request }) => {
  await openDetail(page);
  await expect(page.getByLabel("Client-facing address")).toHaveValue(/\/live\/long\/client\/q2-report$/);
  await page.getByRole("button", { name: "Copy URL" }).click();

  const password = page.getByLabel("New client password");
  await page.getByRole("button", { name: "Generate" }).click();
  await expect(password).toHaveAttribute("type", "text");
  await expect(password).toHaveValue(/^[A-Za-z2-9]{20}$/);
  const generated = await password.inputValue();
  await page.getByRole("button", { name: "Update password" }).click();
  // A password is confirmed by being shown once, not by a four-second toast: the
  // help text promises it is never shown again, so this is the only chance to
  // read it.
  await expect(page.getByRole("dialog", { name: "New client password" })).toBeVisible();
  await page.getByRole("dialog", { name: "New client password" }).getByRole("button", { name: "Done" }).click();

  await page.getByRole("button", { name: "Clear password" }).click();
  const clear = page.getByRole("dialog", { name: "Clear client access?" });
  await expect(clear).toContainText("Anyone a client has open stops working immediately");
  await clear.getByRole("button", { name: "Clear password" }).click();
  await expect(page.locator(".subpanel").filter({ hasText: "Password" }).getByText("Staff-only", { exact: true })).toBeVisible();

  const events = (await (await request.get("/__fixture/events")).json()).events.filter((event) => event.path.endsWith("/password"));
  expect(events[0].body.password).toBe(generated);
  expect(events[1].body).toEqual({ password: "" });
});

// #156: five rows, one commit model. Every editable row saves itself on change
// and reports the outcome in its own row, so nothing on this panel leaves a
// reader wondering whether their change is stored.
const SAVED = /^Saved · /;
const settingRow = (page, name) => page.locator(`.setting-row[data-setting="${name}"]`);

test("every Settings row commits the same way and reports it; delete stays confirmed", async ({ page }) => {
  await openDetail(page);
  // The two commit verbs are gone with the buttons that carried them.
  await expect(page.locator(".setting-list").getByRole("button", { name: /Save|Apply/ })).toHaveCount(0);

  // Every reporting row has both slots in the tree from the start, empty. A
  // role="alert" that is inserted already carrying its text announces nothing, and
  // an aria-describedby pointing at a node that is not there describes nothing.
  for (const name of ["title", "theme", "approval", "availability"]) {
    await expect(settingRow(page, name).getByRole("alert"), `${name} keeps its alert slot`).toHaveCount(1);
    await expect(settingRow(page, name).getByRole("alert")).toHaveText("");
    await expect(settingRow(page, name).getByRole("status")).toHaveText("");
  }
  // The theme select's accessible name is the label a reader can see beside it,
  // and only that: an aria-label would win the name and rename the control.
  await expect(page.locator("#page-theme")).not.toHaveAttribute("aria-label", /./);

  // A title commits on Enter, or on leaving the field having changed it.
  await page.getByLabel("Page title").fill("Updated review title");
  await page.getByLabel("Page title").press("Enter");
  await expect(settingRow(page, "title").getByRole("status")).toHaveText(SAVED);
  await expect(page.getByRole("heading", { level: 1, name: "Updated review title" })).toBeVisible();

  // The theme select no longer needs an "Apply" beside it, and reports itself the
  // same way the title does.
  await page.getByLabel("Theme", { exact: true }).selectOption("client-brand");
  await expect(settingRow(page, "theme").getByRole("status")).toHaveText(SAVED);

  // The approval gate always saved on change; what it never did was say so.
  // "Approval required" / "Open publishing" were pills in the page header, but an
  // approval gate is a setting, not a state a client sees. The control plus its
  // status line is the record of it now.
  await page.getByLabel("Require approval").uncheck();
  await expect(page.getByLabel("Require approval")).not.toBeChecked();
  await expect(settingRow(page, "approval").getByRole("status")).toHaveText(SAVED);
  await expect(page.getByText("Open publishing", { exact: true })).toHaveCount(0);

  // Availability is a switch with exactly that treatment — the state is readable
  // instead of inferable from a button naming the opposite — and #148's takedown
  // confirmation is unchanged.
  const availability = page.getByLabel("Page enabled");
  const badge = page.locator(".page-overview__statuses .badge").first();
  await availability.uncheck();
  const takedown = page.getByRole("dialog", { name: "Take /long/client/q2-report down?" });
  await expect(takedown).toContainText("gets an error page until it is enabled again");
  await takedown.getByRole("button", { name: "Disable page" }).click();
  await expect(badge).toHaveText("Disabled");
  await expect(availability).not.toBeChecked();
  await expect(settingRow(page, "availability").getByRole("status")).toHaveText(SAVED);
  // Putting it back restores service; nothing to confirm.
  await availability.check();
  await expect(badge).toHaveText("Live · gated");
  await expect(availability).toBeChecked();
  await expect(page.getByRole("dialog")).toHaveCount(0);

  // The copy holds in both states, because a takedown does not re-render this
  // panel and could not correct it if it did.
  await expect(settingRow(page, "availability")).toContainText("On serves the published version");

  // Source commits nothing, so it is the one row with no status line to report.
  await expect(settingRow(page, "source").getByRole("status")).toHaveCount(0);
  await expect(page.locator("#edit-source")).toHaveClass(/btn-ghost/);

  const dangerZone = page.getByRole("region", { name: "Danger zone" });
  await expect(dangerZone).toContainText("version history remains recoverable");
  const deletePage = dangerZone.getByRole("button", { name: "Delete page" });
  await expect(deletePage).toHaveClass(/btn-danger/);
  await expect(deletePage).not.toHaveClass(/btn-danger-solid/);
  await deletePage.click();
  const deleteDialog = page.getByRole("dialog", { name: "Delete /long/client/q2-report?" });
  await expect(deleteDialog).toContainText("history remains recoverable");
  await expect(deleteDialog.getByRole("button", { name: "Delete page" })).toHaveClass(/btn-danger-solid/);
  await deleteDialog.locator(".ui-dialog__actions").getByRole("button", { name: "Cancel" }).click();
  await expect(deleteDialog).toBeHidden();
});

test("a failed delete restores the button whole, icon and all", async ({ page }) => {
  await openDetail(page);
  await page.route("**/api/v1/admin/pages/**/delete", (route) =>
    route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) }));

  const remove = page.getByRole("region", { name: "Danger zone" }).getByRole("button", { name: "Delete page" });
  const icons = await remove.locator("svg").count();
  expect(icons, "the danger-zone button carries an icon; that is the point of this test").toBe(1);

  await remove.click();
  await page.getByRole("dialog", { name: "Delete /long/client/q2-report?" })
    .getByRole("button", { name: "Delete page" }).click();

  // request()'s error contract: the server's own words reach the toast.
  await expect(page.getByRole("alert").filter({ hasText: "boom" })).toBeVisible();
  await expect(remove).toBeEnabled();
  await expect(remove).not.toHaveAttribute("aria-busy", "true");
  await expect(remove).toHaveText("Delete page");
  await expect(remove.locator("svg")).toHaveCount(1);
});

test("a settings control stays shut for the whole commit, so it cannot send a second POST", async ({ page }) => {
  await openDetail(page);
  // A settings commit is "POST, report, refetch". Both halves are invisible
  // against a local fixture, so hold each one open and look inside it: a control
  // that is live in there is a duplicate POST waiting to be sent.
  let releasePost = () => {};
  let releaseGet = () => {};
  const heldPost = new Promise((resolve) => { releasePost = resolve; });
  const heldGet = new Promise((resolve) => { releaseGet = resolve; });
  await page.route("**/api/v1/admin/pages/long/client/q2-report/theme", async (route) => {
    await heldPost;
    return route.continue();
  });
  await page.route("**/api/v1/admin/pages/long/client/q2-report", async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    await heldGet;
    return route.continue();
  });

  const posts = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/theme")) posts.push(request.url());
  });

  const theme = page.getByLabel("Theme", { exact: true });
  const row = settingRow(page, "theme");
  await theme.selectOption("client-brand");
  // Progress is stated where the change was made, not in a corner of the screen.
  await expect(row.getByRole("status")).toHaveText("Saving…");
  await expect(row).toHaveAttribute("aria-busy", "true");
  await expect(theme).toBeDisabled();
  await theme.click({ force: true, timeout: 2000 }).catch(() => {});
  expect(posts.length, "a disabled select cannot open, so it cannot send a second /theme POST").toBe(1);

  // The POST has landed and the refetch has not: the row already says "Saved",
  // and the control must still be shut until the screen catches up.
  releasePost();
  await expect(row.getByRole("status")).toHaveText(SAVED);
  await expect(theme).toBeDisabled();

  releaseGet();
  await expect(theme).toBeEnabled();
  await expect(row).not.toHaveAttribute("aria-busy", "true");
  expect(posts.length, "the refetch must not have replayed the commit").toBe(1);
});

test("a mutation button stays busy until its own reload replaces it", async ({ page }) => {
  await openDetail(page);
  // #146 on the mutate() path — publish, approve, reject, rollback and this one.
  // The POST and the reload after it are both invisible against a local fixture,
  // so hold the reload open and look inside it: a button that is live in there is
  // a duplicate mutation waiting to be sent.
  let releaseGet = () => {};
  const heldGet = new Promise((resolve) => { releaseGet = resolve; });
  await page.route("**/api/v1/admin/pages/long/client/q2-report", async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    await heldGet;
    return route.continue();
  });
  const posts = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/password")) posts.push(request.url());
  });

  const clear = page.locator("#clear-password");
  await clear.click();
  await page.getByRole("dialog", { name: "Clear client access?" })
    .getByRole("button", { name: "Clear password" }).click();

  await expect(clear).toBeDisabled();
  await expect(clear).toHaveAttribute("aria-busy", "true");
  await expect(clear).toHaveText("Working…");
  await expect.poll(() => posts.length, { message: "the first POST is in" }).toBe(1);
  await clear.click({ force: true, timeout: 2000 }).catch(() => {});
  expect(posts.length, "a disabled button cannot send a second /password POST").toBe(1);

  releaseGet();
  await expect(page.locator(".subpanel").filter({ hasText: "Password" })
    .getByText("Staff-only", { exact: true })).toBeVisible();
  expect(posts.length, "the reload must not have replayed the mutation").toBe(1);
});

test("a failed save reports the server's own words in the row and keeps what was typed", async ({ page }) => {
  await openDetail(page);
  await page.route("**/api/v1/admin/pages/**/title", (route) =>
    route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "Title service is down" }) }));

  const row = settingRow(page, "title");
  const title = page.getByLabel("Page title");
  await title.fill("A title the server will refuse");
  await title.press("Enter");

  // An alert, not a line of muted help text, and next to the control it belongs to.
  const failure = row.getByRole("alert");
  // "Not saved" in words, not in colour alone: in the dark theme this line and the
  // progress line above it are two pale tints of the same lightness.
  await expect(failure).toHaveText("Not saved — Title service is down");
  await expect(failure).toHaveClass(/field-error/);
  await expect(title).toHaveAttribute("aria-describedby", await failure.getAttribute("id"));
  await expect(title).toHaveAttribute("aria-invalid", "true");
  // No stale "Saved" left standing beside it, and the typed value survives so the
  // reader can try again instead of retyping.
  await expect(row.getByRole("status")).toHaveText("");
  await expect(title).toHaveValue("A title the server will refuse");
  await expect(title).toBeEnabled();

  // Moving focus through a refused field is not a retry. It used to be: the
  // equality guard was switched off in the error state, so every blur re-sent the
  // value the server had just rejected, to a mutating endpoint, with nothing typed.
  const posts = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/title")) posts.push(request.url());
  });
  for (let pass = 0; pass < 3; pass += 1) {
    await title.focus();
    await page.getByLabel("Require approval").focus();
  }
  expect(posts.length, "focus passing through an errored field sends nothing").toBe(0);

  // Enter is the retry, and it clears the alert without clearing the node.
  await page.unroute("**/api/v1/admin/pages/**/title");
  await title.press("Enter");
  // Counted after the row reports it, so the count is not racing the request event.
  await expect(row.getByRole("status")).toHaveText(SAVED);
  expect(posts.length, "Enter is the gesture that retries").toBe(1);
  await expect(row.getByRole("alert")).toHaveText("");
  await expect(row.getByRole("alert")).toHaveCount(1);
  await expect(title).not.toHaveAttribute("aria-invalid", "true");
});

test("reporting a refusal does not move the panel out from under a pending click", async ({ page }) => {
  await openDetail(page);
  // The click that leaves the title field lands on the switch below it. A row that
  // grows or collapses while it reports swallows that click: the operator gets a
  // validation message they did not ask for and no approval gate change, with
  // nothing anywhere saying the gesture was lost.
  const approval = page.getByLabel("Require approval");
  await expect(approval).toBeChecked();
  await page.getByLabel("Page title").fill("");
  await approval.click();
  await expect(settingRow(page, "title").getByRole("alert")).toHaveText(/Enter a title/);
  await expect(approval, "the click that caused the blur still reached the switch").not.toBeChecked();
  await expect(settingRow(page, "approval").getByRole("status")).toHaveText(SAVED);

  // And the other direction: leaving a field that is already sitting on a refusal
  // must not collapse the message and swallow the next click either.
  await page.getByLabel("Page title").focus();
  await approval.click();
  await expect(approval).toBeChecked();
  await expect(settingRow(page, "approval").getByRole("status")).toHaveText(SAVED);
});

test("a commit that lands after an unrelated rebuild reports into the row on the screen", async ({ page }) => {
  await openDetail(page);
  let releasePost = () => {};
  const heldPost = new Promise((resolve) => { releasePost = resolve; });
  await page.route("**/api/v1/admin/pages/long/client/q2-report/title", async (route) => {
    await heldPost;
    return route.continue();
  });

  const title = page.getByLabel("Page title");
  await title.fill("Landed on the server");
  await title.press("Enter");
  await expect(settingRow(page, "title").getByRole("status")).toHaveText("Saving…");

  // Setting a password rebuilds Settings while the title POST is still open. The
  // row the reader is now looking at is not the one the commit started in.
  // Applying a theme is the unrelated commit that rebuilds Settings from what the
  // server holds. (This used to set a password, but #168 scoped that to the header
  // badges and the access card — a password has no bearing on Settings, and
  // rebuilding it there re-minted a preview token for nothing.)
  await page.getByLabel("Theme", { exact: true }).selectOption("client-brand");
  await expect(settingRow(page, "theme").getByRole("status")).toHaveText(SAVED);
  await expect(settingRow(page, "title").getByRole("status")).toHaveText("Saving…");

  releasePost();
  // Both halves have to catch up: the row must not be left saying "Saving…", and
  // the field must not be left showing the value the rebuild read before the
  // commit landed while its own row and the header say it is saved.
  await expect(settingRow(page, "title").getByRole("status")).toHaveText(SAVED);
  await expect(page.getByRole("heading", { level: 1, name: "Landed on the server" })).toBeVisible();
  await expect(page.getByLabel("Page title")).toHaveValue("Landed on the server");
  await expect(settingRow(page, "title")).not.toHaveAttribute("aria-busy", "true");
  await expect(page.getByLabel("Page title")).toBeEnabled();
});

test("a refusal of what was typed does not outlive the value it was about", async ({ page }) => {
  await openDetail(page);
  const row = settingRow(page, "title");
  await page.getByLabel("Page title").fill("");
  await page.getByLabel("Page title").press("Enter");
  await expect(row.getByRole("alert")).toHaveText(/Enter a title/);

  // A client-side refusal belongs to the value that was typed. Left standing on a
  // field that is now fine, it would be read out through aria-describedby on every
  // visit to it.
  //
  // This used to be triggered by setting a password, which re-rendered all four
  // sections. #168 scoped a password change to the header badges and the access
  // card — it has no bearing on Settings — so the rebuild-from-server path is no
  // longer reachable from this row, and the guarantee is pinned the way it is
  // actually reached: give the field a value it is happy with.
  await page.getByLabel("Page title").fill("A title it is happy with");
  await page.getByLabel("Page title").press("Enter");
  await expect(row.getByRole("status")).toHaveText(SAVED);
  await expect(row.getByRole("alert")).toHaveText("");
  await expect(page.getByLabel("Page title")).not.toHaveAttribute("aria-invalid", "true");

  // A refusal from the server is a different thing: nobody has dealt with it, so
  // it survives the rebuild exactly as a "Saved" does.
  await page.route("**/api/v1/admin/pages/**/title", (route) =>
    route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "Title service is down" }) }));
  await page.getByLabel("Page title").fill("A title the server will refuse");
  await page.getByLabel("Page title").press("Enter");
  await expect(row.getByRole("alert")).toHaveText("Not saved — Title service is down");
  await page.getByRole("button", { name: "Generate" }).click();
  await page.getByRole("button", { name: "Update password" }).click();
  await expect(row.getByRole("alert")).toHaveText("Not saved — Title service is down");
});

test("an empty title is refused by the row instead of being saved", async ({ page }) => {
  await openDetail(page);
  const posts = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/title")) posts.push(request.url());
  });

  const row = settingRow(page, "title");
  await page.getByLabel("Page title").fill("   ");
  await page.getByLabel("Page title").press("Enter");
  await expect(row.getByRole("alert")).toHaveText(/Enter a title/);
  await expect(row.getByRole("status")).toHaveText("");
  expect(posts.length, "a blank title is never sent").toBe(0);
  await expect(page.getByRole("heading", { level: 1, name: /North America Programmatic/ })).toBeVisible();
});

test("a switch the server refuses goes back to the state the server is in", async ({ page }) => {
  await openDetail(page);
  await page.route("**/api/v1/admin/pages/**/approval", (route) =>
    route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "Gate is locked" }) }));

  // #170 left this checkbox as the only record of the gate, so it must never sit
  // showing a state that was rejected.
  // .click(), not .uncheck(): putting the switch back is the behaviour under test,
  // and uncheck()'s own "did the state change?" check races the revert.
  const approval = page.getByLabel("Require approval");
  await approval.click();
  await expect(settingRow(page, "approval").getByRole("alert")).toHaveText("Not saved — Gate is locked");
  await expect(approval).toHaveAttribute("aria-invalid", "true");
  await expect(approval).toBeChecked();
});

test("cancelling a takedown changes nothing and says nothing was saved", async ({ page }) => {
  await openDetail(page);
  const posts = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/disable")) posts.push(request.url());
  });

  const availability = page.getByLabel("Page enabled");
  await availability.uncheck();
  await page.getByRole("dialog", { name: "Take /long/client/q2-report down?" })
    .getByRole("button", { name: "Cancel" }).click();
  await expect(availability).toBeChecked();
  await expect(settingRow(page, "availability").getByRole("status")).toHaveText("");
  expect(posts.length, "a cancelled takedown sends nothing").toBe(0);
});

test("a row's saved status survives a rebuild of the whole Settings section", async ({ page }) => {
  await openDetail(page);
  const row = settingRow(page, "title");
  await page.getByLabel("Page title").fill("Saved before the rebuild");
  await page.getByLabel("Page title").press("Enter");
  await expect(row.getByRole("status")).toHaveText(SAVED);

  // Setting a password re-renders all four sections, Settings included. A "Saved"
  // that vanishes there is a reader being told their change was never stored.
  // Applying a theme is the unrelated commit that rebuilds Settings from what the
  // server holds. (This used to set a password, but #168 scoped that to the header
  // badges and the access card — a password has no bearing on Settings, and
  // rebuilding it there re-minted a preview token for nothing.)
  await page.getByLabel("Theme", { exact: true }).selectOption("client-brand");
  await expect(settingRow(page, "theme").getByRole("status")).toHaveText(SAVED);
  await expect(page.getByLabel("Page title")).toHaveValue("Saved before the rebuild");
  await expect(row.getByRole("status")).toHaveText(SAVED);
});

test("source editor warns on unsaved close and saves lossless source with mode and note", async ({ page, request }) => {
  await openDetail(page);
  // #167: the editor now opens from the Review workspace, on the version under
  // review — version 6 here — and no longer on whatever happens to be live.
  const edit = page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" });
  await edit.click();
  let editor = page.getByRole("dialog", { name: "Edit source" });
  const source = editor.getByLabel("HTML source");
  await expect(source).toHaveValue(/Pending fixture source/);
  await source.fill("<!doctype html><html><body><h1>Unsaved</h1></body></html>");
  await page.keyboard.press("Escape");
  let warning = page.getByRole("dialog", { name: "Discard unsaved source changes?" });
  await warning.locator(".ui-dialog__actions").getByRole("button", { name: "Cancel" }).click();
  await expect(editor).toBeVisible();
  await page.keyboard.press("Escape");
  warning = page.getByRole("dialog", { name: "Discard unsaved source changes?" });
  await warning.getByRole("button", { name: "Discard changes" }).click();
  await expect(editor).toBeHidden();
  await expect(edit).toBeFocused();

  await edit.click();
  editor = page.getByRole("dialog", { name: "Edit source" });
  const exactSource = "<!doctype html><html><body><script>window.chart = true;</script><h1>Lossless</h1></body></html>";
  await editor.getByLabel("HTML source").fill(exactSource);
  await editor.getByLabel("Render mode").selectOption("raw");
  await editor.getByLabel("Version note").fill("Preserve chart source exactly");
  await editor.getByRole("button", { name: "Save as new version" }).click();
  await expect(page.locator('.version-option[aria-current="true"]')).toContainText("Version 7");
  await expect(page.locator("#preview-status")).toHaveText("Previewing version 7");

  const events = (await (await request.get("/__fixture/events")).json()).events;
  const deploy = events.find((event) => event.path.endsWith("/deploy-source"));
  expect(deploy.body).toEqual({ html: exactSource, render_mode: "raw", note: "Preserve chart source exactly" });
});

// ── #167: editing the version under review ─────────────────────────────────

test("the review workspace edits the selected version, and Settings edits what is live", async ({ page }) => {
  await openDetail(page);
  const reviewEdit = page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" });

  // The default selection is the newest pending version; the editor opens ITS
  // html, which the detail payload does not carry — it is read per version.
  await reviewEdit.click();
  let editor = page.getByRole("dialog", { name: "Edit source" });
  await expect(editor.getByLabel("HTML source")).toHaveValue(/Pending fixture source/);
  await expect(editor).toContainText("Starting from version 6");
  await editor.getByRole("button", { name: "Cancel" }).click();
  await expect(editor).toBeHidden();
  await expect(reviewEdit).toBeFocused();

  // Select a different version and the editor follows the selection.
  await page.getByRole("button", { name: "All versions (6)" }).click();
  await page.locator(".version-option").filter({ hasText: "Version 4" }).click();
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
  editor = page.getByRole("dialog", { name: "Edit source" });
  await expect(editor.getByLabel("HTML source")).toHaveValue(/Draft fixture source/);
  await expect(editor).toContainText("Starting from version 4");
  // The seed carries its own render mode, not the live one: version 4 is raw.
  await expect(editor.getByLabel("Render mode")).toHaveValue("raw");
  await editor.getByRole("button", { name: "Cancel" }).click();

  // Settings keeps the live entry, named so it cannot be confused with the one
  // above, and it still opens the published html no matter what is selected.
  const liveEdit = page.getByRole("button", { name: "Edit live source" });
  await expect(page.getByRole("button", { name: "Edit source", exact: true })).toHaveCount(1);
  await liveEdit.click();
  const liveEditor = page.getByRole("dialog", { name: "Edit live source" });
  await expect(liveEditor.getByLabel("HTML source")).toHaveValue(/Published fixture source/);
  await expect(liveEditor).toContainText("Starting from the live version 2");
  await expect(liveEditor.getByLabel("Render mode")).toHaveValue("themed");
  await expect(liveEditor).toContainText("Saving creates a new version from this exact HTML");
  await liveEditor.getByRole("button", { name: "Cancel" }).click();

  // …and "the live version" is a fact about the seed, not about which button was
  // pressed: select the live version in the queue and the review editor names it
  // exactly as Settings does, rather than inventing a second name for one act.
  await page.locator(".version-option").filter({ hasText: "Version 2" }).click();
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
  const sameLive = page.getByRole("dialog", { name: "Edit live source" });
  await expect(sameLive.getByLabel("HTML source")).toHaveValue(/Published fixture source/);
  await expect(sameLive).toContainText("Starting from the live version 2");
  await sameLive.getByRole("button", { name: "Cancel" }).click();
  await expectNoSeriousAxeViolations(page, "detail source editors");
});

test("the editor offers one save action, with the note leading the meta row", async ({ page }) => {
  await openDetail(page);
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
  const editor = page.getByRole("dialog", { name: "Edit source" });
  const actions = editor.locator(".ui-dialog__actions");
  await expect(actions.getByRole("button")).toHaveCount(2);
  await expect(actions.getByRole("button", { name: "Cancel" })).toBeVisible();
  await expect(actions.getByRole("button", { name: "Save as new version" })).toBeVisible();
  await expect(editor.getByRole("button", { name: "Save draft" })).toHaveCount(0);
  await expect(editor.getByRole("button", { name: "Save & preview" })).toHaveCount(0);

  // The note is the audit trail, so it reads before the render mode and says so.
  const order = await editor.evaluate((dialog) => {
    const labels = [...dialog.querySelectorAll(".ui-dialog__body .field-label")].map((node) => node.textContent);
    return labels;
  });
  expect(order).toEqual(["HTML source", "Version note", "Render mode"]);
  await expect(editor.getByText("Kept in this page's history")).toBeVisible();
  // The help line does not promise a record the field does not enforce: a blank
  // note is still saved, as "Inline edit", and it says so.
  await expect(editor.getByText('Left blank, it is recorded as “Inline edit”.')).toBeVisible();
  // Plain words, not jargon: no "losslessly", no "Flag foundations".
  await expect(editor.getByText("Themed adds the Elcano look; Raw serves your HTML unchanged.")).toBeVisible();
  await expect(editor.getByText("losslessly")).toHaveCount(0);

  // The note takes the meta row's wide column and Render mode its narrow one, and
  // the source box gives back height rather than pushing them off the bottom. A
  // full-width note row plus a flat 52dvh box put Render mode below the dialog's
  // fold at 1440x900, and the overlay scrollbar gave no hint it was down there.
  await expect(editor.getByLabel("Version note")).toBeVisible();
  await expect(editor.getByLabel("Render mode")).toBeVisible();
  for (const height of [720, 800, 900, 1000]) {
    await page.setViewportSize({ width: 1440, height });
    const row = await editor.evaluate((dialog) => {
      const bodyEl = dialog.querySelector(".ui-dialog__body");
      const body = bodyEl.getBoundingClientRect();
      const fields = [...dialog.querySelectorAll(".editor-meta > .field")];
      const [note, mode] = fields.map((node) => node.getBoundingClientRect());
      return {
        // Two children: a one-child two-column grid would be dead layout inviting
        // the next reader to fill a slot the design no longer has.
        fields: fields.length,
        sameRow: Math.abs(note.top - mode.top) < 2,
        noteWider: note.width > mode.width,
        // Nothing hidden and nothing to scroll to: help lines included.
        bothWholeOnScreen: note.bottom <= body.bottom + 1 && mode.bottom <= body.bottom + 1,
        scrolls: bodyEl.scrollHeight > bodyEl.clientHeight + 1,
      };
    });
    expect(row, `1440x${height}`).toEqual({ fields: 2, sameRow: true, noteWider: true, bothWholeOnScreen: true, scrolls: false });
  }
});

test("a page with no versions at all is invited to write its first source", async ({ page }) => {
  await page.goto("/admin/client-04");
  await expect(page.getByRole("heading", { level: 1, name: "Client 04 operations dashboard" })).toBeVisible();
  // Nothing is selected, so the Review workspace offers no per-version editor.
  await expect(page.getByRole("button", { name: "Edit source", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Edit live source" })).toHaveCount(0);

  const write = page.getByRole("button", { name: "Write source" });
  await expect(page.getByText("This page has no source yet. Write its first HTML by hand.")).toBeVisible();
  await write.click();
  const editor = page.getByRole("dialog", { name: "Write source" });
  await expect(editor.getByLabel("HTML source")).toHaveValue("");
  await expect(editor).toContainText("This page has no source yet, so this starts from an empty file");
  await expect(editor).toContainText("creates this page's first version");
  await expect(editor.getByRole("button", { name: "Save as new version" })).toBeVisible();
  await expectNoSeriousAxeViolations(page, "detail write-source editor");
});

test("a page with versions but nothing live is not told it has no source", async ({ page }) => {
  // client-20 is approval-gated with its only version still pending: it HAS
  // source, none of it is live. Claiming "first version" here would be false and
  // would invite a reviewer to retype what is already sitting in the queue.
  await page.goto("/admin/client-20");
  await expect(page.getByRole("heading", { level: 1, name: "Client 20 operations dashboard" })).toBeVisible();
  await expect(page.getByText("This page has no source yet.")).toHaveCount(0);
  await expect(page.getByText("Nothing is published yet. Write a new version by hand, or edit one from the review queue above.")).toBeVisible();

  // The version in review is editable from the Review workspace, seeded with its
  // own html — that is the route the settings copy points at.
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
  const reviewEditor = page.getByRole("dialog", { name: "Edit source" });
  await expect(reviewEditor.getByLabel("HTML source")).toHaveValue(/Gated fixture source/);
  await expect(reviewEditor).toContainText("Starting from version 1");
  await reviewEditor.getByRole("button", { name: "Cancel" }).click();

  await page.getByRole("button", { name: "Write source" }).click();
  const editor = page.getByRole("dialog", { name: "Write source" });
  await expect(editor.getByLabel("HTML source")).toHaveValue("");
  await expect(editor).toContainText("the versions already in review are left untouched");
  await expect(editor).not.toContainText("first version");
  await expectNoSeriousAxeViolations(page, "detail write-source editor with versions in review");
});

test("a version whose source cannot be read reports it and leaves the button usable", async ({ page }) => {
  await openDetail(page);
  await page.route("**/api/v1/admin/pages/**/versions/106", (route) =>
    route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "source store unavailable" }) }));
  const edit = page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" });
  await edit.click();
  // Filtered, because every reporting Settings row now keeps an empty role="alert"
  // slot in the tree: a live region that is inserted already carrying its text
  // announces nothing, so the nodes exist before there is anything to say.
  await expect(page.getByRole("alert").filter({ hasText: "Couldn't open version 6" }))
    .toContainText("Couldn't open version 6: source store unavailable");
  await expect(page.getByRole("dialog", { name: "Edit source" })).toHaveCount(0);
  await expect(edit).toBeEnabled();
  await expect(edit).toHaveText("Edit source");
  // The trigger is disabled for the length of the read, which drops focus to
  // <body>, and no dialog opened to take it. A keyboard user gets it back rather
  // than being silently moved to the top of the document by a failure.
  await expect(edit).toBeFocused();
});

for (const width of [480, 568, 667]) {
  test(`dialogs keep forms and actions reachable at ${width}x320`, async ({ page }) => {
    await page.setViewportSize({ width, height: 320 });
    await openDetail(page);
    await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
    const editor = page.getByRole("dialog", { name: "Edit source" });
    await expect(editor).toBeVisible();

    const geometry = await editor.evaluate((dialog) => {
      const dialogBox = dialog.getBoundingClientRect();
      const surface = dialog.querySelector(".ui-dialog__surface");
      const body = dialog.querySelector(".ui-dialog__body");
      return {
        viewportHeight: document.documentElement.clientHeight,
        dialogTop: dialogBox.top,
        dialogBottom: dialogBox.bottom,
        surfaceDisplay: getComputedStyle(surface).display,
        surfaceClientWidth: surface.clientWidth,
        surfaceScrollWidth: surface.scrollWidth,
        surfaceClientHeight: surface.clientHeight,
        surfaceScrollHeight: surface.scrollHeight,
        bodyClientHeight: body.clientHeight,
      };
    });
    expect(geometry.dialogTop, JSON.stringify(geometry)).toBeGreaterThanOrEqual(0);
    expect(geometry.dialogBottom, JSON.stringify(geometry)).toBeLessThanOrEqual(geometry.viewportHeight + 1);
    expect(geometry.surfaceDisplay, JSON.stringify(geometry)).toBe("block");
    expect(geometry.surfaceScrollWidth, JSON.stringify(geometry)).toBeLessThanOrEqual(geometry.surfaceClientWidth + 1);
    expect(geometry.surfaceScrollHeight, JSON.stringify(geometry)).toBeGreaterThan(geometry.surfaceClientHeight);
    expect(geometry.bodyClientHeight, JSON.stringify(geometry)).toBeGreaterThan(160);

    await editor.getByLabel("Version note").scrollIntoViewIfNeeded();
    await expect(editor.getByLabel("Version note")).toBeVisible();
    await editor.getByLabel("Render mode").scrollIntoViewIfNeeded();
    await expect(editor.getByLabel("Render mode")).toBeVisible();
    const saveVersion = editor.getByRole("button", { name: "Save as new version" });
    await saveVersion.scrollIntoViewIfNeeded();
    await expect(saveVersion).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(editor).toBeHidden();

    await page.goto("/admin");
    await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();
    await page.getByRole("button", { name: "New page" }).click();
    const create = page.getByRole("dialog", { name: "New page" });
    await create.getByLabel("Title (optional)").scrollIntoViewIfNeeded();
    await expect(create.getByLabel("Title (optional)")).toBeVisible();
    await create.getByLabel("Workspace").scrollIntoViewIfNeeded();
    await expect(create.getByLabel("Workspace")).toBeVisible();
    const createPage = create.getByRole("button", { name: "Create page" });
    await createPage.scrollIntoViewIfNeeded();
    await expect(createPage).toBeVisible();
    await create.getByRole("button", { name: "Cancel" }).click();
    await expect(create).toBeHidden();
    await expectNoHorizontalOverflow(page);
  });
}

test("the detail keeps the product's section nav and says where the page sits", async ({ page, request }) => {
  // The trail is already correct in the server-rendered document, before any
  // script runs — asserted on the raw HTML because the fixture answers the
  // payload too fast to observe the pre-hydration state in the browser.
  const shell = await (await request.get(DETAIL)).text();
  expect(shell).toContain('aria-label="Breadcrumb"');
  expect(shell).toContain(">/long/client/q2-report<");

  await page.goto(DETAIL);
  const nav = page.getByRole("navigation", { name: "Pages sections" });
  await expect(nav).toBeVisible();
  await expect(nav.getByRole("link", { name: "Client pages" })).toHaveAttribute("aria-current", "page");
  await expect(nav.getByRole("link", { name: "Template library" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Partner portals" })).toBeVisible();

  const trail = page.getByRole("navigation", { name: "Breadcrumb" });
  await expect(trail.getByRole("link", { name: "Client pages" })).toHaveAttribute("href", "/admin");
  // Once loaded the trail names the page and its workspace the way a person would.
  await expect(page.locator("#breadcrumb-workspace")).toHaveText("Campaign operations");
  await expect(page.locator("#breadcrumb-current")).toHaveText(/North America Programmatic/);
  await expect(page).toHaveTitle(/^North America Programmatic .+ · Pages$/);
  await expectNoSeriousAxeViolations(page, "detail section nav and breadcrumb");
});

test.describe("with scripting off", () => {
  test.use({ javaScriptEnabled: false });
  test("the server-rendered trail stands on its own", async ({ page }) => {
    await page.goto(DETAIL);
    // No empty crumb, no orphan separator: the workspace crumb stays hidden
    // until the payload can name it.
    await expect(page.locator("#breadcrumb-workspace")).toBeHidden();
    await expect(page.locator(".breadcrumb__list")).toHaveText(/Client pages\s*\/?\s*\/long\/client\/q2-report/);
    await expect(page.getByRole("navigation", { name: "Pages sections" })).toBeVisible();
  });
});

test("a page with no workspace still gets a complete trail", async ({ page, request }) => {
  await request.post("/__fixture/empty-workspaces");
  await page.goto(DETAIL);
  await expect(page.locator("#breadcrumb-workspace")).toHaveText("Ungrouped");
});

test("a confirmation is header, message, actions — with no empty band between them", async ({ page }) => {
  await openDetail(page);
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Reject" }).click();
  const confirmation = page.getByRole("dialog", { name: /Reject version/ });
  await expect(confirmation).toBeVisible();
  // makeDialog always appends a body; a confirmation puts its whole message in the
  // header, so the body would otherwise render as a padded band fenced by two
  // borders — a form that looks like it failed to load.
  await expect(confirmation.locator(".ui-dialog__body")).toBeHidden();
  await expect(confirmation.getByText("Rejected versions remain in history")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(confirmation).toBeHidden();

  // A dialog that does use its body keeps it.
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
  const editor = page.getByRole("dialog", { name: "Edit source" });
  await expect(editor).toBeVisible();
  await expect(editor.locator(".ui-dialog__body")).toBeVisible();
});

test("page switcher handles long lists and nested neighbors", async ({ page }) => {
  await openDetail(page);
  await expect(page.getByLabel(/Switch admin page/).locator("option")).toHaveCount(37);
  await expect(page.locator("#page-switcher-count")).toHaveText(/of 37/);
  await page.locator("#page-switcher-prev").click();
  await expect(page).toHaveURL(/\/admin\/client-35$/);
  await page.locator("#page-switcher-next").click();
  await expect(page).toHaveURL(/\/admin\/long\/client\/q2-report$/);
});

for (const width of [390, 820, 1440]) {
  test(`the version queue shows every row's status and date in full at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await openDetail(page);
    const list = page.locator(".version-list");
    await expect(list).toBeVisible();

    // The list is the scroll container for the rows. An implicit `auto` grid track
    // was floored at the widest row's min-content, so rows overhung the panel and
    // `.version-browser { overflow: hidden }` sheared the badge and date off — the two
    // facts a reviewer opens this list for. Assert the track never exceeds the box.
    const listBox = await list.evaluate((node) => ({ client: node.clientWidth, scroll: node.scrollWidth }));
    expect(listBox.scroll).toBeLessThanOrEqual(listBox.client);

    // And assert it at the pixel level for the row with the longest author on record:
    // every badge and timestamp must sit inside the panel's content box.
    const panelRight = await page.locator(".version-browser").evaluate((node) => node.getBoundingClientRect().right);
    const rows = list.locator(".version-option");
    const count = await rows.count();
    expect(count).toBeGreaterThan(0);
    for (let index = 0; index < count; index += 1) {
      const row = rows.nth(index);
      for (const part of [row.locator(".badge"), row.locator("time")]) {
        const right = await part.evaluate((node) => node.getBoundingClientRect().right);
        expect(right).toBeLessThanOrEqual(panelRight);
      }
    }
  });
}

test("a long author address ellipses instead of pushing the timestamp out of the row", async ({ page }) => {
  await openDetail(page);
  const row = page.locator(".version-option").filter({ hasText: "Version 6" });
  const author = row.locator(".version-option__meta span").first();
  // The full address stays reachable in the title attribute; only the rendering clips.
  await expect(author).toHaveAttribute("title", "review-agent-with-a-long-address@elcanotek.com");
  const clipped = await author.evaluate((node) => node.scrollWidth > node.clientWidth);
  expect(clipped).toBe(true);
  const time = row.locator("time");
  const shown = await time.evaluate((node) => node.scrollWidth <= node.clientWidth + 1);
  expect(shown).toBe(true);
});

test("the page switcher can be browsed with the keyboard without navigating away", async ({ page }) => {
  await openDetail(page);
  const select = page.getByLabel(/Switch admin page/);
  await expect(select).toBeEnabled();
  const start = page.url();

  // On a focused, closed <select> an arrow key both moves the selection and fires
  // `change`. Navigating on `change` alone meant the first press left the page, so
  // the fourth entry — let alone the thirty-seventh — was unreachable by keyboard.
  await select.focus();
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("ArrowUp");
  expect(page.url()).toBe(start);
  await expect(page.locator("#page-switcher-count")).toHaveText("Press Enter to open");
  const pending = await select.inputValue();
  expect(pending).not.toBe("long/client/q2-report");

  // Enter commits.
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/admin/${pending.replace(/\//g, "/")}$`));
});

test("abandoning a keyboard browse leaves the switcher describing the page you are on", async ({ page }) => {
  await openDetail(page);
  const select = page.getByLabel(/Switch admin page/);
  await select.focus();
  await page.keyboard.press("ArrowUp");
  await expect(page.locator("#page-switcher-count")).toHaveText("Press Enter to open");

  // Tab away without committing: the control must not keep claiming a page you
  // never opened.
  await page.keyboard.press("Tab");
  await expect(page.locator("#page-switcher-count")).toHaveText("37 of 37");
  await expect(select).toHaveValue("long/client/q2-report");
  expect(page.url()).toContain("/admin/long/client/q2-report");
});

test("choosing a page with the pointer still navigates immediately", async ({ page }) => {
  await openDetail(page);
  // selectOption fires `change` without any preceding keydown — the pointer path.
  await page.getByLabel(/Switch admin page/).selectOption("client-01");
  await expect(page).toHaveURL(/\/admin\/client-01$/);
});

test("nothing a client can see changes without a confirmation that says so", async ({ page, request }) => {
  await openDetail(page);
  const actions = page.locator(".preview-toolbar .version-actions");

  // Approving an approval-gated version moves the live pointer. It used to be the
  // only client-visible action with no checkpoint, while Reject — which changes
  // nothing a client sees — had one.
  await actions.getByRole("button", { name: "Approve & publish", exact: true }).click();
  const approve = page.getByRole("dialog", { name: "Approve and publish version 6?" });
  await expect(approve).toBeVisible();
  await expect(approve).toContainText("replaces live version 2");
  await expect(approve).toContainText("for everyone with the client link");
  await expect(approve).toContainText("version 2 stays available to roll back to");
  await approve.getByRole("button", { name: "Cancel" }).click();

  // Cancelling means cancelling: no request was sent.
  const events = (await (await request.get("/__fixture/events")).json()).events;
  expect(events.filter((event) => event.path.includes("/approve"))).toHaveLength(0);
  await expect(page.getByText("Live version 2", { exact: true })).toBeVisible();
});

test("the first version a page ever serves is described as such", async ({ page, request }) => {
  // With nothing published there is no pointer to move "from", and no earlier
  // version to promise a rollback to.
  await page.route("**/api/v1/admin/pages/long/client/q2-report", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    await route.fulfill({ json: { ...body, page: { ...body.page, published_version_id: null }, published: null } });
  });
  await page.goto(DETAIL);
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Approve & publish", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: /Approve and publish/ });
  await expect(dialog).toContainText("is the first version this page serves");
  await expect(dialog).not.toContainText("stays available to roll back to");
});

test("an unwanted draft can be discarded, not only published", async ({ page, request }) => {
  await openDetail(page);
  await page.getByRole("button", { name: /All versions/ }).click();
  await page.locator(".version-option").filter({ hasText: "Version 4" }).click();
  const actions = page.locator(".preview-toolbar .version-actions");

  // The draft's only affordance used to be the button that makes it live.
  await expect(actions.getByRole("button", { name: "Publish", exact: true })).toBeVisible();
  const discard = actions.getByRole("button", { name: "Discard draft" });
  await expect(discard).toBeVisible();
  await discard.click();

  // A draft was never submitted for review, so it is discarded, not rejected —
  // and nothing a client sees changes.
  const dialog = page.getByRole("dialog", { name: "Discard version 4?" });
  await expect(dialog).toContainText("can no longer be published");
  await expect(dialog).toContainText("Nothing a client sees changes");
  await dialog.getByRole("button", { name: "Discard draft" }).click();

  const events = (await (await request.get("/__fixture/events")).json()).events;
  expect(events.find((event) => event.path.endsWith("/versions/104/reject"))).toBeTruthy();
  await page.getByRole("button", { name: /All versions/ }).click();
  await expect(page.locator(".version-option").filter({ hasText: "Version 4" })).toContainText(/rejected/i);
  // The live pointer is untouched.
  await expect(page.getByText("Live version 2", { exact: true })).toBeVisible();
});

test("a pending version is still rejected, in the reviewer's words", async ({ page }) => {
  await openDetail(page);
  await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Reject" }).click();
  const dialog = page.getByRole("dialog", { name: "Reject version 6?" });
  await expect(dialog).toContainText("Rejected versions remain in history");
  await expect(dialog.getByRole("button", { name: "Reject version" })).toBeVisible();
});

test("clicking a version keeps everything you have typed elsewhere on the screen", async ({ page }) => {
  await openDetail(page);
  // The one input on this screen that is expensive to lose.
  const password = page.getByLabel("New client password");
  await password.fill("a-password-i-just-generated");
  const title = page.getByLabel("Page title");
  await title.fill("A title I am part way through");

  // Glance at the queue and pick another version — the thing this screen is for.
  await page.getByRole("button", { name: /All versions/ }).click();
  await page.locator(".version-option").filter({ hasText: "Version 4" }).click();
  await expect(page.locator("#preview-status")).toContainText("version 4");

  // The password survives: choosing a version rebuilds the review section and
  // nothing else. The title survives too, but for a different reason — leaving
  // the field IS the commit now (#156), and the row says so rather than leaving a
  // reader to guess whether it stuck.
  await expect(password).toHaveValue("a-password-i-just-generated");
  await expect(title).toHaveValue("A title I am part way through");
  await expect(settingRow(page, "title").getByRole("status")).toHaveText(SAVED);
});

test("re-rendering for an unrelated reason does not re-mint a preview token", async ({ page, request }) => {
  await openDetail(page);
  await expect(page.locator("#preview-status")).toContainText("Previewing version 6");

  const tokensSoFar = async () => {
    const { events } = await (await request.get("/__fixture/events")).json();
    return events.filter((event) => event.path.endsWith("/preview-token")).length;
  };
  const before = await tokensSoFar();
  expect(before).toBeGreaterThan(0);

  // Saving a title has no bearing on which version is being previewed.
  await page.getByLabel("Page title").fill("Renamed while reviewing");
  await page.getByLabel("Page title").press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "Renamed while reviewing" })).toBeVisible();

  expect(await tokensSoFar(), "a settings save must not reload the sandboxed preview").toBe(before);
  await expect(page.locator("#preview-status")).toContainText("Previewing version 6");

  // Choosing the version that is already selected is likewise a no-op.
  await page.locator('.version-option[aria-current="true"]').click();
  expect(await tokensSoFar()).toBe(before);
});

test("a theme change does reload the preview, because it changes how the page renders", async ({ page, request }) => {
  await openDetail(page);
  await expect(page.locator("#preview-status")).toContainText("Previewing version 6");
  const tokensSoFar = async () => {
    const { events } = await (await request.get("/__fixture/events")).json();
    return events.filter((event) => event.path.endsWith("/preview-token")).length;
  };
  const before = await tokensSoFar();
  await page.getByLabel("Theme", { exact: true }).selectOption("client-brand");
  await expect(settingRow(page, "theme").getByRole("status")).toHaveText(SAVED);
  await expect.poll(tokensSoFar, { timeout: 8000 }).toBeGreaterThan(before);
});

test("a keyboard operator keeps their place through an action", async ({ page }) => {
  await openDetail(page);
  // Approve the selected version from the keyboard, and stay somewhere useful.
  const approve = page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Approve & publish", exact: true });
  await approve.focus();
  await expect(approve).toBeFocused();
  await page.keyboard.press("Enter");
  await page.getByRole("dialog", { name: /Approve and publish/ })
    .getByRole("button", { name: "Approve & publish", exact: true }).click();
  await expect(page.getByText("Live version 6", { exact: true })).toBeVisible();

  // The button that was focused is gone with the pending row, so focus lands on
  // the nominated fallback — never on <body>, which is 6+ tab stops from the queue.
  const landed = await page.evaluate(() => {
    const node = document.activeElement;
    return { tag: node.tagName, body: node === document.body, id: node.id, cls: node.className };
  });
  expect(landed.body, `focus fell to <body> (${JSON.stringify(landed)})`).toBe(false);

  // And a commit made from the keyboard hands focus straight back to the control
  // that made it — the input is disabled for the length of the save, which blurs
  // it, so nothing but commitSetting can put a keyboard operator back.
  const title = page.getByLabel("Page title");
  await title.fill("Focus check");
  await title.press("Enter");
  await expect(page.getByRole("heading", { level: 1, name: "Focus check" })).toBeVisible();
  await expect(title).toBeFocused();

  // Tabbing out of the field commits it too, and that must NOT drag focus back
  // out of wherever the operator went.
  await title.fill("Focus check two");
  await title.press("Tab");
  await expect(settingRow(page, "title").getByRole("status")).toHaveText(SAVED);
  await expect(page.getByLabel("Theme", { exact: true })).toBeFocused();
});

test("the page header says the state and the work, not the settings", async ({ page }) => {
  await openDetail(page);
  const statuses = page.locator(".page-overview__statuses");
  // "Enabled" is every page's default state; badging it was noise. An approval
  // gate is a setting and belongs in Settings.
  await expect(statuses.getByText("Enabled", { exact: true })).toHaveCount(0);
  await expect(statuses.getByText("Approval required", { exact: true })).toHaveCount(0);
  await expect(statuses.getByText("Open publishing", { exact: true })).toHaveCount(0);

  // What is left: the state a client sees, which version is serving, and the work
  // waiting on a human — as a link into the queue, not a fourth equal pill.
  await expect(statuses.locator(".badge").first()).toHaveText("Live · gated");
  await expect(statuses).toContainText("Live version 2");
  const todo = statuses.getByRole("link", { name: /2 versions waiting for review/ });
  await expect(todo).toBeVisible();
  await expect(todo).toHaveAttribute("href", "#review-title");
});

test("an approved version that is not serving is not styled as an unreviewed draft", async ({ page }) => {
  await openDetail(page);
  await page.getByRole("button", { name: /All versions/ }).click();
  const approved = page.locator(".version-option").filter({ hasText: "Version 1" }).locator(".badge");
  await expect(approved).toHaveText("Approved");
  await expect(approved).toHaveClass(/badge--live/);
  const draft = page.locator(".version-option").filter({ hasText: "Version 4" }).locator(".badge");
  await expect(draft).toHaveText("Draft");
  await expect(draft).toHaveClass(/badge--draft/);
});

test("the review stage can answer how a client sees this on a phone", async ({ page }) => {
  await openDetail(page);
  await expect(page.locator("#preview-status")).toContainText("Previewing version 6");
  const frame = page.locator("#preview-frame");
  const stage = page.locator(".preview-stage");
  const full = await frame.boundingBox();

  // The chip belongs to the admin, so it sits in the admin's own bar rather than
  // floating over the client's content in the corner of the render.
  const chipInBar = await page.locator("#preview-status").evaluate((node) => Boolean(node.closest(".preview-frame-bar")));
  expect(chipInBar, "the status chip must not float over the rendered page").toBe(true);

  await page.getByRole("button", { name: "Mobile" }).click();
  await expect(stage).toHaveAttribute("data-viewport", "mobile");
  await expect(page.getByRole("button", { name: "Mobile" })).toHaveAttribute("aria-pressed", "true");
  // The width animates, so settle before measuring.
  await expect.poll(async () => Math.round((await frame.boundingBox()).width), { timeout: 5000 }).toBe(390);
  expect((await frame.boundingBox()).width, "the mobile width must narrow the frame").toBeLessThan(full.width);

  // The surrounding admin does not move — the width is set on the frame.
  const shellWidth = await page.locator(".preview-workspace").evaluate((node) => node.getBoundingClientRect().width);
  await page.getByRole("button", { name: "Desktop" }).click();
  await expect(stage).toHaveAttribute("data-viewport", "desktop");
  expect(await page.locator(".preview-workspace").evaluate((node) => node.getBoundingClientRect().width)).toBe(shellWidth);

  // And the choice survives choosing another version.
  await page.getByRole("button", { name: "Tablet" }).click();
  await page.getByRole("button", { name: /All versions/ }).click();
  await page.locator(".version-option").filter({ hasText: "Version 5" }).click();
  await expect(page.locator(".preview-stage")).toHaveAttribute("data-viewport", "tablet");
});

test("a failed preview looks like a failure, not like a slow one", async ({ page }) => {
  await openDetail(page);
  await page.route("**/preview-token", (route) => route.fulfill({ status: 500, json: { error: "token minting is down" } }));
  await page.getByRole("button", { name: "Reload preview" }).click();

  const panel = page.locator("#preview-state");
  await expect(panel).toContainText("Preview unavailable");
  // .preview-stage .state-panel is more specific than .state-panel--error and used
  // to repaint the failure as the neutral loading surface.
  await expect(panel).toHaveClass(/state-panel--error/);
  const painted = await panel.evaluate((node) => {
    const own = getComputedStyle(node);
    return { border: own.borderTopWidth, bg: own.backgroundColor };
  });
  expect(painted.border, "the error panel must keep its own border").not.toBe("0px");
  await expect(page.locator("#preview-status")).toContainText("Preview failed");
});

test("an error toast can be read at leisure; a success toast gets out of the way", async ({ page }) => {
  await openDetail(page);
  await page.getByRole("button", { name: /All versions/ }).click();
  await page.locator(".version-option").filter({ hasText: "Version 1" }).click();

  // Roll back: the confirmation closes first, so the failure lands with no dialog
  // over it. (A modal <dialog> is in the top layer, above any z-index, so a toast
  // behind one cannot be reached — which is why every dialog on these screens
  // reports its own failure inline rather than relying on a toast.)
  await page.route("**/rollback", (route) => route.fulfill({ status: 503, json: { error: "the pointer service is down" } }));
  await page.getByRole("button", { name: "Roll back" }).click();
  await page.getByRole("dialog", { name: /Roll back to version 1/ }).getByRole("button", { name: "Roll back" }).click();

  const failure = page.locator(".toast--error");
  await expect(failure).toBeVisible();
  await expect(failure).toContainText("the pointer service is down");
  await expect(failure.getByRole("button", { name: "Dismiss" })).toBeVisible();

  // An error toast is the only record that something did NOT happen, so it has to
  // outlive the four seconds a success receipt gets.
  await page.waitForTimeout(5000);
  await expect(failure, "an error toast must outlive the success timeout").toBeVisible();
  await failure.getByRole("button", { name: "Dismiss" }).click();
  await expect(failure).toBeHidden();
});

test("both themes and reduced motion remain accessible", async ({ page }) => {
  await openDetail(page);
  for (const theme of ["dark", "light"]) {
    await page.evaluate((value) => localStorage.setItem("flag-theme-preference", value), theme);
    await page.reload();
    await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
    await expect(page.locator("#preview-status")).toContainText("Previewing");
    await expectNoSeriousAxeViolations(page, `detail ${theme} theme`);
  }
  await page.emulateMedia({ reducedMotion: "reduce" });
  const transition = await page.getByRole("button", { name: "Reload preview" }).evaluate((node) => getComputedStyle(node).transitionDuration);
  expect(parseFloat(transition)).toBeLessThanOrEqual(0.00001);
});

for (const width of [320, 390, 768, 1440]) {
  test(`detail has no body-level overflow at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await openDetail(page);
    await expect(page.locator("#preview-status")).toContainText("Previewing");
    if (width === 320) {
      await page.locator(".preview-toolbar .version-actions").getByRole("button", { name: "Edit source" }).click();
      await expect(page.getByRole("dialog", { name: "Edit source" })).toBeVisible();
    }
    await expectNoHorizontalOverflow(page);
  });
}

test("a refused clipboard falls back, and only says so in words a person wrote", async ({ page }) => {
  await openDetail(page);

  // writeText does not only go missing, it REJECTS — a non-secure origin, a tab
  // that lost focus mid-await, Safari without a fresh gesture. The old code fell
  // back only when the API was absent, so the rejection reached the toast as the
  // browser's own exception text.
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: () => Promise.reject(new Error("Failed to execute 'writeText' on 'Clipboard': Write permission denied.")),
      },
    });
  });
  await page.reload();
  await expect(page.getByRole("button", { name: "Copy URL" })).toBeVisible();
  await page.getByRole("button", { name: "Copy URL" }).click();

  // The older execCommand path still works in this browser, so the copy succeeds.
  await expect(page.getByRole("status").filter({ hasText: "Live URL copied" })).toBeVisible();
  await expect(page.locator(".toast--error")).toHaveCount(0);
});

test("when nothing can copy, the message is a sentence, not an exception", async ({ page }) => {
  await openDetail(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: () => Promise.reject(new Error("Failed to execute 'writeText' on 'Clipboard': Write permission denied.")) },
    });
    document.execCommand = () => false;
  });
  await page.reload();
  await page.getByRole("button", { name: "Copy URL" }).click();

  const failure = page.locator(".toast--error");
  await expect(failure).toBeVisible();
  await expect(failure).toContainText("Select the value and copy it with your keyboard");
  // The thing this issue is actually about: no browser exception text on screen.
  await expect(failure).not.toContainText("writeText");
  await expect(failure).not.toContainText("Failed to execute");
});

test("a password is shown once, and Generate does not leave the field readable", async ({ page }) => {
  await openDetail(page);
  // By id, not by label: the field is labelled "New client password" only while
  // the page HAS one, and an earlier test in this file clears it.
  const password = page.locator("#client-password");
  await expect(password).toHaveAttribute("type", "password");

  // Generate used to flip the input to type="text" and never flip it back.
  await page.getByRole("button", { name: "Generate" }).click();
  await expect(password).toHaveAttribute("type", "text");
  await expect(page.getByRole("button", { name: "Hide" })).toHaveAttribute("aria-pressed", "true");

  // Showing the value is its own decision, and it is reversible.
  await page.getByRole("button", { name: "Hide" }).click();
  await expect(password).toHaveAttribute("type", "password");
  await expect(page.getByRole("button", { name: "Show" })).toHaveAttribute("aria-pressed", "false");
  await page.getByRole("button", { name: "Show" }).click();
  await expect(password).toHaveAttribute("type", "text");

  const generated = await password.inputValue();
  expect(generated.length).toBeGreaterThan(8);
  await page.getByRole("button", { name: /^(Update|Set) password$/ }).click();

  // The help text promises the password is never shown again, which is exactly why
  // it has to be shown once — a four-second toast plus a clipboard write that may
  // have been overwritten is not a record.
  const shown = page.getByRole("dialog", { name: /client password/i });
  await expect(shown).toBeVisible();
  await expect(shown).toContainText(generated);
  await expect(shown).toContainText("only time Pages will show it");
  await expect(shown).toContainText("Client link:");
  await shown.getByRole("button", { name: "Done" }).click();

  // And the field is back to being a password field, emptied.
  await expect(password).toHaveValue("");
  await expect(password).toHaveAttribute("type", "password");
});

test("clearing a password is not standing beside the things that set one", async ({ page }) => {
  await openDetail(page);
  const actions = page.locator(".password-actions");
  await expect(actions.getByRole("button", { name: "Generate" })).toBeVisible();
  await expect(actions.getByRole("button", { name: "Update password" })).toBeVisible();
  // It is the opposite action; beside the primary it read as a fourth way to save.
  await expect(actions.getByRole("button", { name: "Clear password" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Clear password" })).toBeVisible();
});

// ── in-page section navigation (#172) ────────────────────────────────────────
// The detail is one continuous column and the app header carries no in-page
// navigation, so Settings sat ~4,000px below Review with nothing but the scroll
// bar to get there. These pin the bar's four promises: it is reachable from the
// top by keyboard, it stays on screen for the whole scroll, it lands a link
// where the reader can read it, and it says which section they are in.

async function navGeometry(page, headingId) {
  return page.evaluate((id) => {
    const heading = document.getElementById(id);
    const overline = heading.closest(".section-heading").querySelector(".overline");
    const nav = document.getElementById("detail-nav").getBoundingClientRect();
    const header = document.querySelector(".page-header").getBoundingClientRect();
    return {
      navTop: nav.top,
      navBottom: nav.bottom,
      headerBottom: header.bottom,
      headingTop: heading.getBoundingClientRect().top,
      overlineTop: overline.getBoundingClientRect().top,
      viewport: document.documentElement.clientHeight,
      documentHeight: document.documentElement.scrollHeight,
    };
  }, headingId);
}

// Put the reader inside a section the way scrolling would, without touching the
// bar — the marker has to track the scroll, not the click that caused it.
async function scrollIntoSection(page, headingId, offset = 4) {
  await page.evaluate(({ id, delta }) => {
    const heading = document.getElementById(id);
    const margin = parseFloat(getComputedStyle(heading).scrollMarginBlockStart) || 0;
    window.scrollTo(0, window.scrollY + heading.getBoundingClientRect().top - margin + delta);
  }, { id: headingId, delta: offset });
}

test("the section bar reaches Client access and Settings without dragging the preview past", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 780 });
  await openDetail(page);
  const bar = page.locator("#detail-nav");
  // Not "Page sections": lib/shell.js already ships a "Pages sections"
  // landmark, and two navigation landmarks a letter apart are the same
  // landmark to anyone listening.
  await expect(bar).toHaveAttribute("aria-label", "On this page");
  await expect(page.getByRole("navigation", { name: "On this page" })).toHaveCount(1);
  await expect(bar.getByRole("link")).toHaveText(["Review", "Client access", "Settings"]);

  // The screen this is for: ~4,000px of it, one viewport at a time.
  const before = await navGeometry(page, "access-title");
  expect(before.documentHeight, "the tall detail is what makes the bar necessary").toBeGreaterThan(3000);

  await bar.getByRole("link", { name: "Client access" }).click();
  const access = await navGeometry(page, "access-title");
  // The bar is pinned directly under the app header, and the section starts
  // below it — including the overline, which is part of the heading and used to
  // be cropped off by an offset that only cleared the two bars.
  expect(Math.abs(access.navTop - access.headerBottom), JSON.stringify(access)).toBeLessThanOrEqual(2);
  expect(access.overlineTop, JSON.stringify(access)).toBeGreaterThanOrEqual(access.navBottom - 1);
  expect(access.headingTop).toBeGreaterThan(access.navBottom);
  await expect(bar.getByRole("link", { name: "Client access" })).toHaveAttribute("aria-current", "true");

  await bar.getByRole("link", { name: "Settings" }).click();
  const settings = await navGeometry(page, "settings-title");
  expect(settings.overlineTop, JSON.stringify(settings)).toBeGreaterThanOrEqual(settings.navBottom - 1);
  await expect(bar.getByRole("link", { name: "Settings" })).toHaveAttribute("aria-current", "true");

  // And back up: the whole point is that this is symmetric.
  await bar.getByRole("link", { name: "Review" }).click();
  await expect(bar.getByRole("link", { name: "Review" })).toHaveAttribute("aria-current", "true");
  await expect(page.getByRole("heading", { level: 2, name: "Review" })).toBeInViewport();
  await expectNoHorizontalOverflow(page);
  await expectNoSeriousAxeViolations(page, "detail section bar at 390px");
});

test("the section bar stays on screen and above the preview for the whole scroll", async ({ page }) => {
  await openDetail(page);
  await expect(page.locator("#preview-status")).toContainText("Previewing");
  await page.evaluate(() => window.scrollTo(0, 900));
  const stuck = await navGeometry(page, "review-title");
  expect(Math.abs(stuck.navTop - stuck.headerBottom), JSON.stringify(stuck)).toBeLessThanOrEqual(2);

  // The preview iframe is composited. Painted with a backdrop-filter the bar
  // vanished behind it across the entire preview column while still taking the
  // clicks, so assert the bar owns those pixels rather than merely the hit test.
  const covered = await page.evaluate(() => {
    const nav = document.getElementById("detail-nav");
    const rect = nav.getBoundingClientRect();
    const stage = document.querySelector(".preview-stage").getBoundingClientRect();
    const node = document.elementFromPoint((stage.left + stage.right) / 2, rect.top + rect.height / 2);
    return { inside: nav.contains(node), opaque: getComputedStyle(nav).backgroundColor.startsWith("rgb("), tag: node && node.tagName };
  });
  expect(covered.inside, JSON.stringify(covered)).toBe(true);
  expect(covered.opaque, "an overlay over a composited iframe must be opaque").toBe(true);
});

test("the section bar marks the section the reader has scrolled into", async ({ page }) => {
  await openDetail(page);
  const current = page.locator('#detail-nav a[aria-current="true"]');
  // At the top the reader is in the overview panel, which has no link in this
  // bar. Marking Review there would announce a section they have not reached.
  await expect(current).toHaveCount(0);

  await scrollIntoSection(page, "review-title");
  await expect(current).toHaveText("Review");

  await scrollIntoSection(page, "access-title");
  await expect(current).toHaveText("Client access");

  await scrollIntoSection(page, "settings-title");
  await expect(current).toHaveText("Settings");

  // The last section is shorter than the viewport, so its top can never reach
  // the bar; at the end of the document the reader is in it regardless.
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await expect(current).toHaveText("Settings");

  await page.evaluate(() => window.scrollTo(0, 0));
  await expect(current).toHaveCount(0);
  // Exactly one section is ever marked.
  await scrollIntoSection(page, "access-title");
  await expect(page.locator('#detail-nav a[aria-current="true"]')).toHaveCount(1);
});

test("the bar marks the section that was clicked even when the page cannot scroll that far", async ({ page }) => {
  // The end of the document used to be special-cased to the LAST section
  // unconditionally. Any viewport tall enough that Client access is the last
  // thing that can be scrolled to therefore said "Settings" while the reader was
  // looking at the Client access heading, hundreds of pixels up the screen.
  for (const height of [1000, 1400, 1800, 2200]) {
    await page.setViewportSize({ width: 1440, height });
    await openDetail(page);
    for (const label of ["Client access", "Settings", "Review"]) {
      await page.locator("#detail-nav").getByRole("link", { name: label }).click();
      await expect(
        page.locator('#detail-nav a[aria-current="true"]'),
        `clicked ${label} at ${height}px tall`
      ).toHaveText(label);
    }
  }
});

test("a window taller than the whole detail is not reported as the bottom of it", async ({ page }) => {
  // scrollY 0 + innerHeight >= scrollHeight is true of every document that does
  // not scroll at all — a portrait monitor, a zoomed-out tab — so the end-of-
  // document rule marked Settings on a page the reader had not touched.
  await page.setViewportSize({ width: 1440, height: 2400 });
  await page.goto("/admin/client-01");
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const fits = await page.evaluate(() => document.documentElement.scrollHeight <= window.innerHeight + 2);
  expect(fits, "this viewport is meant to contain the whole detail").toBe(true);
  await expect(page.locator('#detail-nav a[aria-current="true"]')).toHaveCount(0);
});

test("focusing a section link draws the whole ring, not the part that survived a clip", async ({ page }) => {
  // overflow-x on the list made both axes a clipping box, and the list is
  // exactly as tall as the link inside it, so --focus-ring's 4px was sliced off
  // top, bottom and left. axe cannot see this; only geometry can.
  await openDetail(page);
  const drawn = await page.evaluate(() => {
    const RING = 4;
    const nav = document.getElementById("detail-nav");
    const navBox = nav.getBoundingClientRect();
    // Any clipping box between the link and the bar takes the ring with it.
    const clippers = [];
    for (const link of document.querySelectorAll(".detail-nav__link")) {
      for (let node = link; node && node !== nav.parentElement; node = node.parentElement) {
        const style = getComputedStyle(node);
        if (style.overflowX !== "visible" || style.overflowY !== "visible") {
          clippers.push(`${node.className || node.tagName}:${style.overflowX}/${style.overflowY}`);
        }
      }
    }
    return {
      clippers,
      // The bar's own padding is what has to hold the ring vertically; the row
      // is the full content width, so sideways it has the page gutter.
      headroom: [...document.querySelectorAll(".detail-nav__link")].map((link) => {
        const rect = link.getBoundingClientRect();
        return { label: link.textContent, top: rect.top - RING - navBox.top, bottom: navBox.bottom - (rect.bottom + RING) };
      }),
    };
  });
  expect(drawn.clippers, "nothing between a section link and the bar may clip its focus ring").toEqual([]);
  for (const link of drawn.headroom) {
    expect(link.top, JSON.stringify(link)).toBeGreaterThanOrEqual(0);
    expect(link.bottom, JSON.stringify(link)).toBeGreaterThanOrEqual(0);
  }
});

test("the anchor offset is the bar’s real height, not a rounded-off guess at it", async ({ page }) => {
  // --pages-detail-nav feeds every heading's scroll-margin. It is written as the
  // tokens that draw the bar rather than a measured round number, so a change to
  // the bar's padding moves the offset with it — this is what holds the two
  // together if someone changes one and not the other.
  await openDetail(page);
  const measured = await page.evaluate(() => {
    const nav = document.getElementById("detail-nav");
    const probe = document.createElement("div");
    probe.style.height = "var(--pages-detail-nav)";
    document.body.append(probe);
    const declared = probe.getBoundingClientRect().height;
    probe.remove();
    return { real: nav.getBoundingClientRect().height, declared, rows: nav.querySelector(".detail-nav__list").getBoundingClientRect().height };
  });
  expect(Math.abs(measured.real - measured.declared), JSON.stringify(measured)).toBeLessThanOrEqual(1);
});

test("screens that share .screen-stack but have no section bar carry no offset for one", async ({ page }) => {
  // The offset was scoped to .screen-stack, which portals and templates also use.
  for (const route of ["/admin/portals", "/admin/templates"]) {
    await page.goto(route);
    await expect(page.locator(".screen-stack")).toBeVisible();
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const offsets = await page.evaluate(() => ({
      bars: document.querySelectorAll("#detail-nav").length,
      margins: [...document.querySelectorAll(".section-heading > h2")].map((h) => getComputedStyle(h).scrollMarginBlockStart),
    }));
    expect(offsets.bars, route).toBe(0);
    for (const margin of offsets.margins) expect(parseFloat(margin) || 0, `${route} ${margin}`).toBe(0);
  }
});

test("a keyboard operator reaches the section bar from the top and lands on the heading", async ({ page }) => {
  await openDetail(page);
  await page.locator(".brand-link").focus();
  const trail = [];
  for (let step = 0; step < 14; step += 1) {
    await page.keyboard.press("Tab");
    const here = await page.evaluate(() => {
      const node = document.activeElement;
      return { nav: node.classList.contains("detail-nav__link"), inApp: Boolean(node.closest("#app")), label: node.textContent.trim().slice(0, 24) };
    });
    trail.push(here.label);
    if (here.nav || here.inApp) {
      expect(here.nav, `the bar must come before the sections in the tab order, got ${JSON.stringify(trail)}`).toBe(true);
      break;
    }
  }
  expect(trail.length, `never reached the bar: ${JSON.stringify(trail)}`).toBeLessThan(14);

  // Following a link with the keyboard has to move the reading position too, or
  // the next Tab carries on from the top of the document.
  await page.keyboard.press("Tab");
  await page.keyboard.press("Tab");
  await expect(page.locator("#detail-nav a:focus")).toHaveText("Settings");
  await page.keyboard.press("Enter");
  await expect(page.locator("#settings-title")).toBeFocused();
  const landed = await navGeometry(page, "settings-title");
  expect(landed.headingTop).toBeGreaterThan(landed.navBottom);
});

test("the section bar survives a partial re-render of the section under it", async ({ page }) => {
  await openDetail(page);
  // #145 rebuilds only the sections a change touches. A bar rendered inside one
  // would be destroyed by an unrelated save — and would move, which is the one
  // thing a fixed point of reference may not do.
  await page.evaluate(() => { document.getElementById("detail-nav").dataset.probe = "same-node"; });
  await page.getByRole("button", { name: "Clear password" }).click();
  await page.getByRole("dialog", { name: "Clear client access?" }).getByRole("button", { name: "Clear password" }).click();
  await expect(page.getByRole("button", { name: "Set password" })).toBeVisible();

  await expect(page.locator("#detail-nav")).toHaveCount(1);
  await expect(page.locator("#detail-nav")).toHaveAttribute("data-probe", "same-node");
  await expect(page.locator("#detail-nav").getByRole("link")).toHaveText(["Review", "Client access", "Settings"]);
  await page.locator("#detail-nav").getByRole("link", { name: "Client access" }).click();
  await expect(page.locator('#detail-nav a[aria-current="true"]')).toHaveText("Client access");
});

test("no section bar is offered when the detail itself failed to load", async ({ page }) => {
  await page.route("**/api/v1/admin/pages/long/client/q2-report", (route) => route.fulfill({ status: 503, json: { error: "detail service is down" } }));
  await page.goto(DETAIL);
  await expect(page.getByText("detail service is down")).toBeVisible();
  // A bar whose three links all scroll nowhere is worse than no bar.
  await expect(page.locator("#detail-nav")).toHaveCount(0);
});
