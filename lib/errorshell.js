// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The dashboard host's 404. It is a PUBLIC surface — anyone who mistypes the host
// reaches it without ever signing in — so it uses the same standalone chrome as
// the content host's gates rather than a second hand-written copy of it. Its only
// difference is where this host serves the Flag files from, and the CTA, which
// only makes sense here.
const standaloneChrome = require("./standalone-chrome");

function notFound() {
  return standaloneChrome.render({
    title: "Not found · Pages",
    kicker: "Not found",
    heading: "Page not found",
    bodyHtml: "<p>This address is not a Pages route.</p>",
    assetsBase: "/shell-assets/flag",
    cta: { href: "/admin", label: "Back to Pages" },
  });
}

module.exports = { notFound };
