// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

import "strings"

// The built-in switcher: a self-contained control for every dashboard that does
// not render its own. Constraints it has to satisfy, since it lands inside an
// arbitrary client design Pages did not write:
//
//   - It must not move anything. `position: fixed`, so it cannot reflow a chart
//     or widen the document at any viewport.
//   - It must be legible over an unknown background. The palette is read from
//     the page's own background at runtime.
//   - Its names must not collide. Everything is prefixed `pgnav`, and the styles
//     are scoped to that class rather than to element selectors.
//   - A native <details> for the disclosure, real <a> elements for the links: the
//     sandbox withholds allow-popups, so a scripted window.open would silently do
//     nothing, and anchors bring keyboard and middle-click for free.
//   - Titles via textContent. A sibling's title is set by whoever owns that page.
//   - Print and reduced-motion are respected, because this is somebody's report.
//
// Inside a shadow root, so: no token fallbacks to get half-applied, no
// specificity contest with the dashboard's own `a:visited`, and a focus ring
// that a design's `*:focus{outline:none}` cannot remove. The palette comes from
// the page's own background rather than from Flag tokens, because a `raw`
// dashboard is served without them and every fallback fired at once — a white
// pill parked on a dark bespoke report.
const switcherCSS = `:host{all:initial;position:fixed;z-index:10000;font:600 13px/1.35 system-ui,-apple-system,"Segoe UI",sans-serif}
*{box-sizing:border-box}
.pgnav{color:var(--fg)}
.pgnav>summary{list-style:none;cursor:pointer;display:inline-flex;align-items:center;gap:7px;padding:7px 11px;border:1px solid var(--edge);border-radius:8px;color:var(--fg);background:var(--bg);box-shadow:0 2px 10px rgba(16,24,40,.18)}
.pgnav>summary::-webkit-details-marker{display:none}
.pgnav>summary::marker{content:""}
.pgnav>summary:hover{border-color:var(--accent)}
.pgnav>summary:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.pgnav__caret{font-size:10px;opacity:.7}
.pgnav__menu{position:absolute;top:calc(100% + 6px);right:0;min-width:15rem;max-width:min(22rem,calc(100vw - 32px));max-height:min(70vh,32rem);overflow:auto;padding:7px;border:1px solid var(--edge);border-radius:10px;background:var(--bg);box-shadow:0 14px 38px rgba(16,24,40,.28)}
.pgnav__head{padding:4px 8px 7px;font-size:10.5px;font-weight:700;letter-spacing:.1em;text-transform:uppercase;color:var(--muted)}
.pgnav__item{display:block;padding:8px;border-radius:6px;color:var(--fg);text-decoration:none;font-weight:500;overflow-wrap:anywhere}
.pgnav__item:hover{background:var(--hover)}
.pgnav__item:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}
.pgnav__item--current{background:var(--hover);font-weight:700}
.pgnav__head--link{display:block;text-decoration:none;border-radius:6px;color:var(--muted)}
.pgnav__head--link:hover{background:var(--hover);text-decoration:underline}
.pgnav__head--link:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}
.pgnav__home{margin-left:6px;font-size:9.5px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;opacity:.7}
@media print{:host{display:none}}
@media(prefers-reduced-motion:reduce){*{transition:none!important}}`

// switcherJSTemplate is the builder. NAV_ID_JSON and CSS_JSON are replaced
// with JSON string literals once, at init. No inline handlers: they are
// evaluated in a scope that cannot see anything declared here, which is this
// codebase's single most repeated authoring bug.
const switcherJSTemplate = `(function(){
  function build(){
    var block=document.getElementById(NAV_ID_JSON);
    if(!block||document.querySelector(".pgnav-host"))return;
    var nav; try{nav=JSON.parse(block.textContent);}catch(e){return;}
    var pages=(nav&&Array.isArray(nav.pages))?nav.pages:[];
    if(pages.length<2)return;

    // The page decides where this sits. A dashboard's own brand row, export
    // button or "last updated" badge lives in the top-right corner too, and on a
    // phone there is no spare corner at all — so below 40rem it goes to the
    // bottom, where a thumb is anyway and designs rarely put anything.
    var root=document.documentElement;
    var rs=getComputedStyle(root);
    function authored(name,fallback){var v=rs.getPropertyValue(name); return v&&v.trim()?v.trim():fallback;}
    var narrow=(window.innerWidth||root.clientWidth||0)<640;
    var host=document.createElement("div"); host.className="pgnav-host";
    // Inline, so a dashboard's own rule on div cannot move it. The two custom
    // properties are the sanctioned way for a design to relocate it.
    host.style.cssText="position:fixed;z-index:10000;"
      +(narrow?("bottom:"+authored("--pages-nav-bottom","12px")+";")
              :("top:"+authored("--pages-nav-top","12px")+";"))
      +"right:"+authored("--pages-nav-right","12px")+";";
    var shadow=host.attachShadow?host.attachShadow({mode:"open"}):null;
    if(!shadow)return;

    // A raw dashboard is served without Flag tokens, so a token-with-fallback
    // palette fired every fallback at once and painted a white pill on a dark
    // report. Read what the page actually is instead.
    var lum=0.85;
    try{
      var probe=getComputedStyle(document.body||root).backgroundColor||"";
      var m=probe.match(/rgba?[(]([^)]+)[)]/);
      if(m){
        var parts=m[1].split(",").map(function(x){return parseFloat(x);});
        if(parts.length<4||parts[3]>0.1){
          lum=(0.2126*parts[0]+0.7152*parts[1]+0.0722*parts[2])/255;
        }
      }
    }catch(e){}
    var dark=lum<0.45;
    var palette=dark
      ? "--bg:#1b2130;--fg:#f2f4f8;--muted:#aab3c4;--edge:rgba(226,232,240,.28);--hover:rgba(226,232,240,.12);--accent:#8fb3e8"
      : "--bg:#fff;--fg:#1a2233;--muted:#5a6577;--edge:rgba(120,130,150,.45);--hover:rgba(120,130,150,.12);--accent:#3f6aa6";
    var style=document.createElement("style");
    // The palette goes AFTER the sheet: :host{all:initial} resets custom properties
    // too, so declaring it first left every var() undefined and painted the light
    // fallback on a dark report — which is the bug this is fixing.
    style.textContent=CSS_JSON+" :host{"+palette+"}";
    shadow.appendChild(style);

    var d=document.createElement("details"); d.className="pgnav";
    var s=document.createElement("summary");
    s.appendChild(document.createTextNode("Page"));
    var c=document.createElement("span"); c.className="pgnav__caret"; c.setAttribute("aria-hidden","true");
    c.textContent="\u25be"; s.appendChild(c); d.appendChild(s);
    var m=document.createElement("nav"); m.className="pgnav__menu";
    // "Dashboard pages" was our filing label, and appending " dashboards" to it
    // made the accessible name read "Dashboard pages dashboards". A partner with
    // a portal gets its name; one without gets the plain possessive.
    var portalName=(nav.portal&&nav.portal.name)?nav.portal.name:"";
    var label=portalName||"Your dashboards";
    m.setAttribute("aria-label",portalName?portalName+" dashboards":"Your dashboards");
    var portalUrl=(nav.portal&&nav.portal.url)?nav.portal.url:"";
    var h;
    if(portalUrl){h=document.createElement("a"); h.className="pgnav__head pgnav__head--link"; h.setAttribute("href",portalUrl);}
    else{h=document.createElement("div"); h.className="pgnav__head";}
    h.textContent=label; m.appendChild(h);
    pages.forEach(function(p){
      var node=document.createElement(p.current?"span":"a");
      node.className="pgnav__item"+(p.current?" pgnav__item--current":"");
      if(p.current){node.setAttribute("aria-current","page");}else{node.setAttribute("href",p.url);}
      node.textContent=p.title||p.slug;
      if(p.home){var hb=document.createElement("span"); hb.className="pgnav__home"; hb.textContent="Start here"; node.appendChild(hb);}
      m.appendChild(node);
    });
    if(nav.truncated){
      var more;
      if(portalUrl){more=document.createElement("a"); more.className="pgnav__head pgnav__head--link"; more.setAttribute("href",portalUrl);}
      else{more=document.createElement("div"); more.className="pgnav__head";}
      more.textContent="See all in your portal"; m.appendChild(more);
    }
    d.appendChild(m);
    // The click lands on the HOST from the document's point of view, so the
    // outside-click test has to ask the host, not the details inside it.
    document.addEventListener("click",function(e){ if(d.open&&!host.contains(e.target))d.open=false; });
    d.addEventListener("keydown",function(e){ if(e.key==="Escape"){d.open=false;s.focus();} });
    shadow.appendChild(d);
    (document.body||document.documentElement).appendChild(host);
  }
  if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",build);
  else build();
})();`

// switcherTags is the pair of head tags (sheet, builder) that make up the
// built-in control, computed once.
var switcherTags = func() []string {
	js := strings.NewReplacer(
		"NAV_ID_JSON", jsonString(NavBlockID),
		"CSS_JSON", jsonString(switcherCSS),
	).Replace(switcherJSTemplate)
	return []string{
		"<style data-flag-injected>\n" + switcherCSS + "\n</style>",
		"<script data-flag-injected>\n" + js + "\n</script>",
	}
}()

func defaultSwitcherTags() []string { return append([]string(nil), switcherTags...) }
