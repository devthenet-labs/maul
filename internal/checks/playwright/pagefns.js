// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT
//
// maul's page functions (SPEC Q9, spike SP-5): one function the support
// module runs on an element through locator.evaluate, so nothing is
// installed in the page and no page script can stand in for it. It
// returns {verdict: "pass" | "fail" | "indeterminate", why: [...]}; an
// indeterminate verdict is a check error (unsure), never a fail.
//
//   visible: a person can see it. A non-zero box whose centre is in the
//            viewport (the caller scrolls it in first), visibility
//            visible, effective opacity above 0.1, and drawn: clipped to
//            nothing (or to at most 1x1 px, a screen-reader span) by an
//            overflow, clip or clip-path inset is a fail, but under a
//            scaled, rotated or zoomed ancestor, or when the hit test
//            finds it at its centre all the same, or for a form control
//            that clips itself away (a screen-reader checkbox behind a
//            styled box), indeterminate. Not
//            found at its centre (another clip-path shape cut it away,
//            say), or covered there by an opaque layer that is not the
//            element or inside it: indeterminate, never a pass.
//            Its own non-emoji text needs a contrast of at least 1.5:1
//            against what is painted under it; with none, the text of its
//            shown descendants is judged so: one passing passes, all
//            failing fails, otherwise indeterminate (bare text a slot
//            shows inside it is not measured: indeterminate).
//            A zero box fails saying what hides its content (an overflow
//            that clips, or nothing in it showing); when its content
//            overflows it and shows all the same (a wrapper whose height
//            collapsed), indeterminate: assert on what shows.
//   text:    which of its text a person can see (the text assertion's
//            measure, defect gate 7): each text node is shown, hidden or
//            unsure by where its own boxes are drawn. Hidden: not
//            rendered (display none, a closed details or other content
//            the browser skips), visibility or
//            opacity, or clipped to nothing (at most 1x1 px) by an
//            overflow hidden or clip, a clip or clip-path inset, or the
//            page's extent. An overflow a person scrolls (auto, scroll)
//            clips nothing here. Unsure: clipped under a scaled, rotated
//            or zoomed ancestor, or by an overflow hidden box that still
//            scrolls (a carousel's other slide). It returns the text as
//            the runner reads it (full), with only what is shown (shown)
//            and with what is shown or unsure (maybe); the support
//            module matches them.
//   legible: WCAG AA contrast of its own non-emoji text, 4.5:1, or 3:1
//            for large text (24px, or 18.66px at weight 700 and over).
//   hit:     the topmost element at the box centre is the element or
//            inside it (clickable's hit test; a pass is confirmed by the
//            caller's trial click).
//   drawn:   the part of the box that is drawn (the box relations' and
//            size's measure): the layout box cut by every ancestor whose
//            overflow clips it, and by the page's own extent (the
//            viewport for a fixed element). A pass carries both boxes and
//            what cut them; an element clipped away entirely is not
//            drawn, which is indeterminate (it measures nothing).
//
// The background is resolved from what is painted under the element: the
// hit-test stack at its centre with pointer-events lifted, the ancestor
// chain only when the element is not in that stack. A gradient is judged
// against each of its colour stops composited over what lies beneath, by
// unanimity: every stop passes, pass; every stop fails, fail; otherwise
// indeterminate. A background image is indeterminate, and so is a layer
// whose paint the arithmetic does not know: a replaced element (<img>,
// <canvas>, <video>, SVG, an embed), a ::before or ::after with a
// background that may lie under the text (placed over the point by its
// insets, or in the flow of a box the text is positioned over or shares
// a grid cell with; one in the flow beside the text does not), and a web
// component's shadow content out of reach (a closed root, found by a
// probe child no root slots, or an open one beside the text). Shadow
// roots are measured through: the stack is the element's own root's, its
// chains run through slots and hosts, and text slotted into a component
// is measured from inside that component's root. Paint effects the
// arithmetic cannot model (text-shadow, mix-blend-mode, filter,
// backdrop-filter) turn a computed fail into indeterminate, and
// background-clip text is always indeterminate. Known miss, never a false
// fail: filter: brightness(0) on a parent darkening text into its
// background reads as a pass.
(el, kind) => {
  const VISIBLE_MIN_CONTRAST = 1.5;
  const VISIBLE_MIN_OPACITY = 0.1;
  const LEGIBLE_MIN_CONTRAST = 4.5;
  const LEGIBLE_MIN_CONTRAST_LARGE = 3;

  // Colours. Chromium serialises computed colours, gradient stops
  // included, as rgb()/rgba(), or color(srgb ...) for color-mix and the
  // like. Anything else is unparsed, hence indeterminate.
  const parseColour = (s) => {
    s = s.trim();
    let m = s.match(/^rgba?\(\s*([\d.]+)[ ,]+([\d.]+)[ ,]+([\d.]+)(?:\s*[,/]\s*([\d.]+%?))?\s*\)$/);
    if (m) {
      const a = m[4] === undefined ? 1 : m[4].endsWith('%') ? parseFloat(m[4]) / 100 : parseFloat(m[4]);
      return { r: +m[1], g: +m[2], b: +m[3], a };
    }
    m = s.match(/^color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*([\d.]+))?\s*\)$/);
    if (m) return { r: m[1] * 255, g: m[2] * 255, b: m[3] * 255, a: m[4] === undefined ? 1 : +m[4] };
    if (s === 'transparent') return { r: 0, g: 0, b: 0, a: 0 };
    return null;
  };
  // over composites fg, with its alpha, over an opaque bg.
  const over = (fg, bg) => {
    const a = fg.a;
    return { r: fg.r * a + bg.r * (1 - a), g: fg.g * a + bg.g * (1 - a), b: fg.b * a + bg.b * (1 - a), a: 1 };
  };
  const lum = (c) => {
    const f = (v) => {
      v /= 255;
      return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  };
  const ratio = (a, b) => {
    const x = lum(a);
    const y = lum(b);
    return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
  };
  const hex = (c) => '#' + [c.r, c.g, c.b].map((v) => Math.round(v).toString(16).padStart(2, '0')).join('');
  const desc = (n) =>
    n.tagName.toLowerCase() + (n.id ? '#' + n.id : '') + (n.classList.length ? '.' + [...n.classList].join('.') : '');

  // gradientStops lists the colour stops of a computed gradient.
  const gradientStops = (img) => {
    const out = [];
    const re = /(rgba?\([^)]*\)|color\(srgb[^)]*\))/g;
    let m;
    while ((m = re.exec(img))) {
      const c = parseColour(m[1]);
      if (c) out.push(c);
    }
    return out;
  };
  const dedupe = (cs) => {
    const seen = new Set();
    return cs.filter((c) => {
      const k = hex(c);
      if (seen.has(k)) return false;
      seen.add(k);
      return true;
    });
  };

  // The canvas when nothing opaque is found: white, or Chromium's dark
  // canvas when the root's color-scheme is dark only.
  const canvasColour = () => {
    const cs = getComputedStyle(document.documentElement).colorScheme || '';
    return /\bdark\b/.test(cs) && !/\blight\b/.test(cs) ? { r: 18, g: 18, b: 18, a: 1 } : { r: 255, g: 255, b: 255, a: 1 };
  };

  // up is n's parent in the rendered (flat) tree: its slot when slotted,
  // the host above a shadow root's top.
  const up = (n) => n.assignedSlot || n.parentElement || (n.parentNode && n.parentNode.host) || null;
  // within reports whether n is a, or inside it in the rendered tree.
  const within = (n, a) => {
    for (; n; n = up(n)) if (n === a) return true;
    return false;
  };
  // rootOf is the document or shadow root that hit-tests n's tree.
  const rootOf = (n) => {
    const r = n.getRootNode();
    return r instanceof ShadowRoot ? r : document;
  };
  // scopesOf lists the roots whose elements root r's hit test shows as
  // themselves: r and every root above its host.
  const scopesOf = (r) => {
    const out = new Set();
    for (; r; r = r instanceof ShadowRoot ? rootOf(r.host) : null) out.add(r);
    return out;
  };
  // insideOf reports whether n is in host h's shadow tree, at any depth.
  const insideOf = (n, h) => {
    for (let r = n.getRootNode(); r instanceof ShadowRoot; r = r.host.getRootNode()) if (r.host === h) return true;
    return false;
  };

  // stackAt is root r's hit-test stack at (x, y), top first, with
  // pointer-events lifted for the one measurement in the document and in
  // every open shadow root (a layer with pointer-events none still paints).
  const LIFT = '*,*::before,*::after{pointer-events:auto!important}';
  const stackAt = (r, x, y) => {
    const roots = [document];
    for (let i = 0; i < roots.length; i++) {
      for (const n of roots[i].querySelectorAll('*')) if (n.shadowRoot) roots.push(n.shadowRoot);
    }
    const sts = roots.map((o) => {
      const st = document.createElement('style');
      st.textContent = LIFT;
      (o === document ? document.documentElement : o).appendChild(st);
      return st;
    });
    try {
      return r.elementsFromPoint(x, y).filter((n) => !sts.includes(n));
    } finally {
      for (const st of sts) st.remove();
    }
  };

  // layersBelow lists the layers painted under t, top first, with the
  // point and the roots they were measured in: the hit-test stack at its
  // centre in t's own root, else the ancestor chain. Text slotted into a
  // component whose own shadow content is out of that root's sight is
  // measured from inside the component's root instead, where both show.
  const layersBelow = (t) => {
    const r = t.getBoundingClientRect();
    const x = r.left + r.width / 2;
    const y = r.top + r.height / 2;
    let root = rootOf(t);
    for (let tries = 0; tries < 8; tries++) {
      const stack = stackAt(root, x, y);
      const i = stack.indexOf(t);
      if (i < 0) break;
      const layers = stack.slice(i);
      const scopes = scopesOf(root);
      const host = layers.find((n) => n !== t && n.shadowRoot && !scopes.has(n.shadowRoot) && within(t, n));
      if (!host) return { layers, x, y, scopes };
      root = host.shadowRoot;
    }
    const layers = [];
    for (let n = t; n && n.nodeType === 1; n = up(n)) layers.push(n);
    return { layers, x, y, scopes: scopesOf(rootOf(t)) };
  };

  // resolveBackground returns the opaque background candidates under t,
  // or why they cannot be known. background-color paints below
  // background-image; a gradient contributes each stop as a candidate.
  const resolveBackground = (t) => {
    const { layers, x, y, scopes } = layersBelow(t);
    const walk = (k) => {
      if (k >= layers.length) return { colours: [canvasColour()] };
      const node = layers[k];
      const unknown = unknownPaint(node, x, y, scopes, t);
      if (unknown) return { indeterminate: unknown };
      const cs = getComputedStyle(node);
      const img = cs.backgroundImage;
      const bg = parseColour(cs.backgroundColor);
      if (!bg) return { indeterminate: 'unparsed background-color on ' + desc(node) };
      const hasImg = img && img !== 'none';
      if (hasImg && /url\(|image-set\(|element\(|paint\(/.test(img)) {
        return { indeterminate: 'background image on ' + desc(node) };
      }
      if (!hasImg && bg.a === 0) return walk(k + 1);
      let base;
      if (bg.a >= 1) base = [bg];
      else {
        const below = walk(k + 1);
        if (below.indeterminate) return below;
        base = below.colours.map((c) => over(bg, c));
      }
      if (!hasImg) return { colours: base };
      const stops = gradientStops(img);
      if (!stops.length) return { indeterminate: 'unparsed background-image on ' + desc(node) };
      const cands = [];
      for (const s of stops) for (const b of base) cands.push(s.a >= 1 ? s : over(s, b));
      return { colours: dedupe(cands) };
    };
    return walk(0);
  };

  // ownText is e's own text, colour emoji left out (they ignore the CSS
  // colour); a form control's value counts as its own text.
  const EMOJI = /[\p{Emoji_Presentation}\u{FE0F}\u{200D}\u{20E3}\u{1F3FB}-\u{1F3FF}]/gu;
  const ownText = (e) => {
    let t = '';
    for (const n of e.childNodes) if (n.nodeType === 3) t += n.data;
    if (
      (e.tagName === 'INPUT' && !/^(checkbox|radio|range|color|file|hidden|image)$/.test(e.type)) ||
      e.tagName === 'TEXTAREA'
    ) {
      t += e.value || e.placeholder || '';
    }
    if (e.tagName === 'SELECT' && e.selectedOptions[0]) t += e.selectedOptions[0].text;
    return t.trim().replace(EMOJI, '').trim();
  };

  const effectiveOpacity = (e) => {
    let o = 1;
    for (let n = e; n && n.nodeType === 1; n = up(n)) o *= parseFloat(getComputedStyle(n).opacity);
    return o;
  };

  // contrast returns the contrast range of t's own text over its
  // background candidates, or why it cannot be known. The text colour's
  // alpha is multiplied by the effective opacity before compositing.
  const contrast = (t) => {
    const cs = getComputedStyle(t);
    const fg = parseColour(cs.color);
    if (!fg) return { indeterminate: 'unparsed color ' + cs.color };
    if (cs.webkitTextFillColor && cs.webkitTextFillColor !== cs.color) {
      const f = parseColour(cs.webkitTextFillColor);
      if (f && f.a === 0) return { indeterminate: 'transparent text fill' };
    }
    const effects = [];
    for (let n = t; n && n.nodeType === 1; n = up(n)) {
      const s = getComputedStyle(n);
      if (/text/.test(s.backgroundClip) || /text/.test(s.webkitBackgroundClip || '')) {
        return { indeterminate: 'background-clip text on ' + desc(n) };
      }
      if (n === t && s.textShadow !== 'none') effects.push('text-shadow');
      if (s.mixBlendMode !== 'normal') effects.push('mix-blend-mode on ' + desc(n));
      if (s.filter !== 'none') effects.push('filter on ' + desc(n));
      if (s.backdropFilter && s.backdropFilter !== 'none') effects.push('backdrop-filter on ' + desc(n));
    }
    const bg = resolveBackground(t);
    if (bg.indeterminate) return bg;
    const fgA = { ...fg, a: fg.a * effectiveOpacity(t) };
    const ratios = bg.colours.map((b) => ratio(over(fgA, b), b));
    const size = parseFloat(cs.fontSize);
    const weight = parseInt(cs.fontWeight, 10) || 400;
    return {
      min: Math.min(...ratios),
      max: Math.max(...ratios),
      effects,
      large: size >= 24 || (size >= 18.66 && weight >= 700),
      fg: hex(over(fgA, bg.colours[0])),
      bg: bg.colours.map(hex),
    };
  };

  // judge decides by unanimity over the candidates; a fail under a paint
  // effect is indeterminate.
  const judge = (c, floor) => {
    const range = () => c.min.toFixed(2) + '..' + c.max.toFixed(2) + ' (' + c.fg + ' on ' + c.bg.join('|') + ')';
    if (c.indeterminate) return { verdict: 'indeterminate', why: [c.indeterminate] };
    if (c.min >= floor) return { verdict: 'pass', why: [] };
    if (c.max < floor && !c.effects.length) return { verdict: 'fail', why: ['contrast ' + range() + ' < ' + floor] };
    if (c.max < floor) return { verdict: 'indeterminate', why: ['contrast ' + range() + ' under ' + c.effects.join(', ')] };
    return { verdict: 'indeterminate', why: ['background colours disagree: contrast ' + range() + ' vs ' + floor] };
  };

  // still reports whether a box with style s sits where the normal flow
  // put it: not positioned away, moved or pulled over a neighbour by one
  // of the negative margins named.
  const still = (s, margins) =>
    (s.position === 'static' ||
      (/^(relative|sticky)$/.test(s.position) &&
        ['top', 'right', 'bottom', 'left'].every((k) => s[k] === 'auto' || s[k] === '0px'))) &&
    s.transform === 'none' &&
    (!s.translate || s.translate === 'none') &&
    margins.every((k) => parseFloat(s[k]) >= 0);
  const MARGINS = ['marginTop', 'marginRight', 'marginBottom', 'marginLeft'];

  // besideFlow reports whether a ::before or ::after of n laid out in the
  // flow (style s) is sure to sit beside t's text, never under it: t is n
  // or in n's normal flow all the way up (a box positioned, floated,
  // moved or pulled by a negative margin may lie over the pseudo-element),
  // the pseudo-element is not pulled over n's content (a ::before by its
  // right or bottom margin, an ::after by its left or top one), and in a
  // grid it is placed automatically, in a cell of its own.
  const besideFlow = (n, which, s, t) => {
    if (!t) return false;
    const pulls = which === '::before' ? ['marginRight', 'marginBottom'] : ['marginLeft', 'marginTop'];
    if (!still(s, pulls) || s.float !== 'none') return false;
    if (
      /grid/.test(getComputedStyle(n).display) &&
      !['gridRowStart', 'gridRowEnd', 'gridColumnStart', 'gridColumnEnd'].every((k) => s[k] === 'auto')
    ) {
      return false;
    }
    for (let a = t; a; a = up(a)) {
      if (a === n) return true;
      const as = getComputedStyle(a);
      if (!still(as, MARGINS) || as.float !== 'none') return false;
    }
    return false;
  };

  // shrinks reports whether a transform only scales the box down about a
  // point inside it (an underline scaled away until hover), so the box it
  // was laid out with still bounds what it paints.
  const shrinks = (s, w, h) => {
    if ((s.translate && s.translate !== 'none') || (s.scale && s.scale !== 'none')) return false;
    if (s.rotate && s.rotate !== 'none') return false;
    if (s.transform === 'none') return true;
    const m = /^matrix\(([^)]*)\)$/.exec(s.transform);
    if (!m) return false;
    const [a, b, c, d, e, f] = m[1].split(',').map(parseFloat);
    const [ox, oy] = s.transformOrigin.split(' ').map(parseFloat);
    return b === 0 && c === 0 && e === 0 && f === 0 && a >= 0 && a <= 1 && d >= 0 && d <= 1 && ox >= 0 && ox <= w && oy >= 0 && oy <= h;
  };

  // pseudoOrigin is the viewport point an absolutely positioned or fixed
  // ::before or ::after of n is placed from: the padding box of its
  // containing block, the page or the viewport; null when that block is
  // an inline box over several lines, or scaled, rotated or zoomed, so its
  // place is not known.
  const pseudoOrigin = (n, position) => {
    let cb = null;
    for (let a = n; a && !cb; a = up(a)) {
      const as = getComputedStyle(a);
      if (establishesFixedBlock(as) || (position === 'absolute' && as.position !== 'static')) cb = a;
    }
    for (let a = cb || n; a; a = up(a)) if (warps(getComputedStyle(a))) return null;
    if (!cb) return position === 'fixed' ? { x: 0, y: 0 } : { x: -scrollX, y: -scrollY };
    const r = cb.getBoundingClientRect();
    const display = getComputedStyle(cb).display;
    if (display === 'contents') return null;
    // An inline block's is its one line box; over more lines it is unknown.
    if (display === 'inline') {
      if (cb.getClientRects().length !== 1) return null;
      const cs = getComputedStyle(cb);
      return { x: r.left + parseFloat(cs.borderLeftWidth), y: r.top + parseFloat(cs.borderTopWidth) };
    }
    return { x: r.left + cb.clientLeft - cb.scrollLeft, y: r.top + cb.clientTop - cb.scrollTop };
  };

  // pseudoAt says whether a ::before or ::after of n with a background or
  // image may paint at (x, y), under t's text when t is given: one laid
  // out in the flow beside that text does not (besideFlow); an absolutely
  // positioned or fixed one is placed by its insets in its containing
  // block; any other is unknown, so it may.
  const pseudoAt = (n, x, y, t) => {
    for (const which of ['::before', '::after']) {
      const s = getComputedStyle(n, which);
      if (/^(none|normal)$/.test(s.content) || s.display === 'none' || s.visibility !== 'visible') continue;
      if (parseFloat(s.opacity) === 0) continue;
      const bg = parseColour(s.backgroundColor);
      if (bg && bg.a === 0 && s.backgroundImage === 'none' && !/url\(|gradient\(/.test(s.content)) continue;
      const may = which + ' of ' + desc(n);
      if (!/^(absolute|fixed)$/.test(s.position)) {
        if (besideFlow(n, which, s, t)) continue;
        return may;
      }
      const o = pseudoOrigin(n, s.position);
      if (!o) return may;
      const border = s.boxSizing === 'border-box';
      const pad = (a, b) =>
        border
          ? 0
          : parseFloat(s['padding' + a]) +
            parseFloat(s['padding' + b]) +
            parseFloat(s['border' + a + 'Width']) +
            parseFloat(s['border' + b + 'Width']);
      const l = o.x + parseFloat(s.left);
      const top = o.y + parseFloat(s.top);
      const w = parseFloat(s.width) + pad('Left', 'Right');
      const h = parseFloat(s.height) + pad('Top', 'Bottom');
      if (![l, top, w, h].every(Number.isFinite) || !shrinks(s, w, h)) return may;
      if (x >= l && x <= l + w && y >= top && y <= top + h) return may;
    }
    return '';
  };

  // closedRoot reports whether n, a defined custom element with no open
  // shadow root, may have a closed one: a child given a slot name no root
  // has is laid out only where there is no shadow root at all.
  const closedRoot = (n) => {
    const p = document.createElement('span');
    p.slot = 'maul-probe-' + Math.random().toString(36).slice(2);
    p.style.cssText =
      'display:block!important;position:absolute!important;visibility:hidden!important;width:1px!important;height:1px!important';
    try {
      n.appendChild(p);
      return p.getClientRects().length === 0;
    } catch {
      return true;
    } finally {
      p.remove();
    }
  };

  // unknownPaint says why what n paints at (x, y) is not known, measured
  // in the roots scopes, under t's text when t is given: a replaced
  // element, a ::before or ::after that may paint at the point, a shadow
  // root out of sight whose content paints there, or a custom element
  // with a closed root. '' when nothing is unknown.
  const REPLACED = /^(IMG|CANVAS|VIDEO|OBJECT|EMBED|IFRAME)$/;
  const unknownPaint = (n, x, y, scopes, t) => {
    if (REPLACED.test(n.tagName) || n instanceof SVGElement || (n.tagName === 'INPUT' && n.type === 'image')) {
      return 'unknown paint of ' + desc(n);
    }
    const p = pseudoAt(n, x, y, t);
    if (p) return 'unknown paint of ' + p;
    if (n.shadowRoot && !scopes.has(n.shadowRoot)) {
      const inner = scopesOf(n.shadowRoot);
      for (const i of stackAt(n.shadowRoot, x, y)) {
        if (insideOf(i, n) && paints(i, x, y, inner)) return 'the shadow content of ' + desc(n);
      }
    } else if (!n.shadowRoot && n.localName.includes('-') && customElements.get(n.localName) && closedRoot(n)) {
      return 'unknown paint of ' + desc(n) + ' (a closed shadow root)';
    }
    return '';
  };

  // paints says what n paints at (x, y), or '' for nothing a person would
  // see: faded out, or no background, image, replaced content or other
  // paint maul cannot rule out (text alone, a floating label, is not a
  // cover).
  const paints = (n, x, y, scopes) => {
    if (effectiveOpacity(n) <= VISIBLE_MIN_OPACITY) return '';
    const unknown = unknownPaint(n, x, y, scopes);
    if (unknown) return unknown;
    const s = getComputedStyle(n);
    const bg = parseColour(s.backgroundColor);
    if (!bg || bg.a > 0) return 'the background of ' + desc(n);
    if (s.backgroundImage !== 'none') return 'the background image of ' + desc(n);
    if (s.backdropFilter && s.backdropFilter !== 'none') return 'the backdrop-filter of ' + desc(n);
    return '';
  };

  // found is where a person would look for el and find it: the centre of
  // its box, or, when that point misses it (a link wrapped over two
  // lines), of the first of its line boxes it is found at, with the
  // layers the hit-test stack has above el there or, for an element that
  // is never hit itself (a table row), above the topmost thing inside it,
  // its own descendants left out. null when it is found at none of them.
  const found = () => {
    const rs = [el.getBoundingClientRect(), ...el.getClientRects()];
    for (const r of rs) {
      if (r.width <= 0 || r.height <= 0) continue;
      const x = r.left + r.width / 2;
      const y = r.top + r.height / 2;
      if (!(x >= 0 && x < innerWidth && y >= 0 && y < innerHeight)) continue;
      const root = rootOf(el);
      const stack = stackAt(root, x, y);
      let i = stack.indexOf(el);
      if (i < 0) i = stack.findIndex((n) => within(n, el));
      if (i >= 0) return { x, y, root, above: stack.slice(0, i).filter((n) => !within(n, el)) };
    }
    return null;
  };

  // covered says what covers el where it is found: a layer above it that
  // paints. Its own descendants are its content, never a cover; '' when
  // nothing does.
  const covered = (at) => {
    for (const n of at.above) {
      const what = paints(n, at.x, at.y, scopesOf(at.root));
      if (what) return 'centre (' + Math.round(at.x) + ',' + Math.round(at.y) + ') covered by ' + what;
    }
    return '';
  };

  // shown reports whether a person could see descendant d at all: a
  // non-zero box, visibility visible, not faded out and drawn beyond a
  // pixel (a screen-reader label is not shown).
  const shown = (d) => {
    const r = d.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) return false;
    if (getComputedStyle(d).visibility !== 'visible' || effectiveOpacity(d) <= VISIBLE_MIN_OPACITY) return false;
    const m = drawn(d);
    return !nothing(m);
  };

  // descendantText judges the text of el's shown descendants as own text
  // is judged: one passing passes, all failing fails, anything else is
  // indeterminate. What a slot inside el shows is its content too: a
  // slotted element is judged with the rest, slotted bare text is
  // indeterminate. With no text at all, the element is seen by its box.
  const DESCENDANTS_MAX = 50;
  const descendantText = () => {
    const verdicts = [];
    let more = false;
    const all = new Set();
    const loose = [];
    const add = (r) => {
      for (const d of r.querySelectorAll('*')) {
        all.add(d);
        if (d.shadowRoot) add(d.shadowRoot);
        if (d.tagName !== 'SLOT') continue;
        for (const a of d.assignedNodes({ flatten: true })) {
          if (a.nodeType === 1 && !all.has(a)) {
            all.add(a);
            if (a.shadowRoot) add(a.shadowRoot);
            add(a);
          } else if (a.nodeType === 3 && a.data.trim().replace(EMOJI, '').trim() && a.parentElement !== el) {
            loose.push(a.parentElement);
          }
        }
      }
    };
    if (el.shadowRoot) add(el.shadowRoot);
    add(el);
    for (const d of all) {
      if (!ownText(d) || !shown(d)) continue;
      if (verdicts.length === DESCENDANTS_MAX) {
        more = true;
        break;
      }
      const v = judge(contrast(d), VISIBLE_MIN_CONTRAST);
      if (v.verdict === 'pass') return v;
      verdicts.push({ ...v, why: v.why.map((w) => desc(d) + ': ' + w) });
    }
    // Bare text a slot shows inside el takes the slot's colour, which is
    // not measured: never a pass on the box alone.
    for (const h of loose.slice(0, 1)) {
      verdicts.push({ verdict: 'indeterminate', why: ['the text ' + desc(h) + ' slots into it is not measured'] });
    }
    if (!verdicts.length) return { verdict: 'pass', why: [] };
    if (!more && verdicts.every((v) => v.verdict === 'fail')) return { verdict: 'fail', why: verdicts[0].why };
    const why = verdicts.find((v) => v.verdict === 'indeterminate');
    return {
      verdict: 'indeterminate',
      why: why ? why.why : ['no readable text among its first ' + DESCENDANTS_MAX + ' text descendants'],
    };
  };

  // nothing reports whether a drawn measure leaves a person nothing to
  // see: clipped away, or to at most a pixel each way.
  const nothing = (m) => m.verdict !== 'pass' || (m.clip.width <= 1 && m.clip.height <= 1);

  const visible = () => {
    const r = el.getBoundingClientRect();
    const why = [];
    let overflows = false;
    if (r.width <= 0 || r.height <= 0) {
      // A zero box shows nothing of itself; what is in it may overflow it
      // and show (a wrapper whose height collapsed), unless it is clipped.
      const s = getComputedStyle(el);
      const clips = [s.overflowX, s.overflowY].filter((o) => o !== 'visible');
      overflows = contentShows();
      why.push(
        'zero box ' + r.width + 'x' + r.height +
          (overflows ? '' : clips.length ? ', its content clipped by its overflow ' + clips[0] : ', nothing in it shows'),
      );
    }
    const cx = r.left + r.width / 2;
    const cy = r.top + r.height / 2;
    if (!(cx >= 0 && cx < innerWidth && cy >= 0 && cy < innerHeight)) {
      why.push('centre (' + Math.round(cx) + ',' + Math.round(cy) + ') outside the viewport');
    }
    const vis = getComputedStyle(el).visibility;
    if (vis !== 'visible') why.push('visibility ' + vis);
    const op = effectiveOpacity(el);
    if (op <= VISIBLE_MIN_OPACITY) why.push('opacity ' + op.toFixed(2) + ' <= ' + VISIBLE_MIN_OPACITY);
    if (why.length === 1 && overflows) {
      return { verdict: 'indeterminate', why: [why[0] + ', yet its content overflows it and shows: assert on that'] };
    }
    if (why.length) return { verdict: 'fail', why };
    const m = drawn(el);
    const at = found();
    if (nothing(m)) {
      const what =
        m.verdict === 'pass'
          ? 'drawn only ' + m.clip.width.toFixed(1) + 'x' + m.clip.height.toFixed(1) + ' px'
          : 'clipped away';
      const where = what + (m.by.length ? ' by ' + m.by.join(', ') : '');
      if (m.warped.length) return { verdict: 'indeterminate', why: [where + ' under ' + m.warped.join(', ')] };
      // A form control that hides itself this way stands behind what a
      // person does see (a styled box, its label), which maul cannot name;
      // one its container clipped away (a collapsed panel) is not shown.
      const own = (r.width <= 1 && r.height <= 1) || (m.by.length > 0 && m.by.every((w) => w === desc(el)));
      if (own && /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName)) {
        return { verdict: 'indeterminate', why: [where + ': a hidden form control, assert on what is shown in its place'] };
      }
      // The arithmetic and the hit test disagree: neither is believed.
      if (at && (r.width > 1 || r.height > 1)) return { verdict: 'indeterminate', why: [where + ', yet found at its centre'] };
      return { verdict: 'fail', why: ['not drawn: ' + where] };
    }
    if (!at) return { verdict: 'indeterminate', why: ['not found at its centre'] };
    const cover = covered(at);
    if (cover) return { verdict: 'indeterminate', why: [cover] };
    if (ownText(el)) return judge(contrast(el), VISIBLE_MIN_CONTRAST);
    return descendantText();
  };

  const legible = () => {
    if (!ownText(el)) return { verdict: 'indeterminate', why: ['no own text (colour emoji are not text)'] };
    const c = contrast(el);
    return judge(c, c.large ? LEGIBLE_MIN_CONTRAST_LARGE : LEGIBLE_MIN_CONTRAST);
  };

  const hit = () => {
    const r = el.getBoundingClientRect();
    const x = r.left + r.width / 2;
    const y = r.top + r.height / 2;
    const top = rootOf(el).elementFromPoint(x, y);
    if (top && within(top, el)) return { verdict: 'pass', why: [] };
    return {
      verdict: 'fail',
      why: ['centre (' + Math.round(x) + ',' + Math.round(y) + ') hits ' + (top ? desc(top) : 'nothing')],
    };
  };

  // establishesFixedBlock reports whether an element with style s is the
  // containing block of its fixed-position descendants.
  const establishesFixedBlock = (s) =>
    s.transform !== 'none' ||
    s.perspective !== 'none' ||
    s.filter !== 'none' ||
    (s.backdropFilter && s.backdropFilter !== 'none') ||
    /\b(paint|layout|strict|content)\b/.test(s.contain) ||
    /\b(transform|perspective|filter)\b/.test(s.willChange);

  // pxOf reads a computed length in px against a reference size for a
  // percentage; NaN when it is neither.
  const pxOf = (v, ref) => {
    v = v.trim();
    if (/^-?[\d.]+px$/.test(v)) return parseFloat(v);
    if (/^-?[\d.]+%$/.test(v)) return (parseFloat(v) / 100) * ref;
    if (v === '0') return 0;
    return NaN;
  };

  // clipOf is the box n's own clip (absolutely positioned only) and
  // clip-path inset() leave of it, in viewport coordinates, or null when
  // neither clips. Any other clip-path shape is left to the hit test: a
  // point it cuts away is not found.
  const clipOf = (n, s) => {
    const r = n.getBoundingClientRect();
    let l = -Infinity;
    let t = -Infinity;
    let rr = Infinity;
    let b = Infinity;
    let clips = false;
    const m = /^rect\((.*)\)$/.exec(s.clip);
    if (m && /^(absolute|fixed)$/.test(s.position)) {
      const v = m[1].split(/\s*,\s*|\s+/);
      const side = (i, ref, base, edge) => (v[i] === 'auto' ? edge : base + pxOf(v[i], ref));
      if (v.length === 4) {
        t = side(0, r.height, r.top, -Infinity);
        rr = side(1, r.width, r.left, Infinity);
        b = side(2, r.height, r.top, Infinity);
        l = side(3, r.width, r.left, -Infinity);
        clips = true;
      }
    }
    if (s.clipPath && s.clipPath !== 'none') {
      const im = /^inset\(([^)]*?)(?:\s+round\s[^)]*)?\)(?:\s+border-box)?$/.exec(s.clipPath);
      const v = im ? im[1].trim().split(/\s+/) : [];
      if (v.length >= 1 && v.length <= 4) {
        const [it, ir = it, ib = it, il = ir] = v;
        t = Math.max(t, r.top + pxOf(it, r.height));
        rr = Math.min(rr, r.right - pxOf(ir, r.width));
        b = Math.min(b, r.bottom - pxOf(ib, r.height));
        l = Math.max(l, r.left + pxOf(il, r.width));
        clips = true;
      }
    }
    if (!clips || [l, t, rr, b].some(Number.isNaN)) return null;
    return { l, t, r: rr, b };
  };

  // warps reports whether style s maps n's own box to the viewport other
  // than by a translation, so its client size and its viewport box
  // disagree and a cut by it is not to be trusted.
  const warps = (s) => {
    if (s.zoom && s.zoom !== '1' && s.zoom !== 'normal') return true;
    if ((s.scale && s.scale !== 'none') || (s.rotate && s.rotate !== 'none')) return true;
    if (s.transform === 'none') return false;
    const m = /^matrix\(([^)]*)\)$/.exec(s.transform);
    if (!m) return true;
    const [a, b, c, d] = m[1].split(',').map(parseFloat);
    return !(a === 1 && b === 0 && c === 0 && d === 1);
  };

  // drawn cuts t's layout box by each ancestor that clips it, following
  // the containing-block chain: an absolutely positioned box escapes the
  // clipping of ancestors below its positioned containing block, a fixed
  // one every ancestor below one with a transform (or the like). The root
  // and body are left to the last cut: the page's scrollable extent, or
  // the viewport for a box fixed to it, so a box below the fold is drawn
  // (a person scrolls the page to it) but one placed off the page is not.
  // clip and clip-path inset() cut too, t's own and its clipping chain's,
  // and an ancestor that scales, rotates or zooms is named in warped,
  // since its cut is then unsure.
  const drawn = (t) => measure(t.getBoundingClientRect(), t, up(t), getComputedStyle(t).position, false);

  // measure cuts the box r as drawn cuts an element's: own is the element
  // whose box it is (its own clip and clip-path cut it; null for a text
  // node's boxes), first the nearest ancestor that may clip it, pos its
  // position. reach is the text measure's: an overflow a person scrolls
  // (auto, scroll) cuts nothing, and loose reports a cut by an overflow
  // hidden box that still scrolls to more, which a script may yet show.
  const measure = (r, own, first, pos, reach) => {
    const box = { x: r.left, y: r.top, width: r.width, height: r.height };
    let left = r.left;
    let top = r.top;
    let right = r.right;
    let bottom = r.bottom;
    const by = [];
    let loose = false;
    const cut = (l, t, rr, b, who) => {
      const cuts = l > left || t > top || rr < right || b < bottom;
      if (cuts) by.push(who);
      left = Math.max(left, l);
      top = Math.max(top, t);
      right = Math.min(right, rr);
      bottom = Math.min(bottom, b);
      return cuts;
    };
    const warped = [];
    const clipBy = (n, s) => {
      const c = clipOf(n, s);
      if (c) cut(c.l, c.t, c.r, c.b, desc(n));
    };
    const root = document.documentElement;
    if (own) clipBy(own, getComputedStyle(own));
    for (let a = first; a && a !== root; a = up(a)) if (warps(getComputedStyle(a))) warped.push(desc(a));
    for (let a = first; a && a !== root && a !== document.body; a = up(a)) {
      const s = getComputedStyle(a);
      if (pos === 'fixed' && !establishesFixedBlock(s)) continue;
      if (pos === 'absolute' && s.position === 'static' && !establishesFixedBlock(s)) continue;
      pos = s.position;
      clipBy(a, s);
      const scrolls = /^(auto|scroll)$/;
      const clipX = s.overflowX !== 'visible' && !(reach && scrolls.test(s.overflowX));
      const clipY = s.overflowY !== 'visible' && !(reach && scrolls.test(s.overflowY));
      if ((!clipX && !clipY) || /^(inline|contents)$/.test(s.display)) continue;
      const ar = a.getBoundingClientRect();
      // overflow: clip paints past the padding box by its clip margin.
      const mg = s.overflowX === 'clip' && s.overflowY === 'clip' ? pxOf(s.overflowClipMargin || '0px', 0) || 0 : 0;
      const l = ar.left + a.clientLeft - mg;
      const t = ar.top + a.clientTop - mg;
      const w = clipX ? a.clientWidth + 2 * mg : Infinity;
      const h = clipY ? a.clientHeight + 2 * mg : Infinity;
      const cuts = cut(clipX ? l : -Infinity, clipY ? t : -Infinity, l + w, t + h, desc(a));
      if (
        cuts &&
        ((clipX && s.overflowX === 'hidden' && a.clientWidth > 1 && a.scrollWidth > a.clientWidth) ||
          (clipY && s.overflowY === 'hidden' && a.clientHeight > 1 && a.scrollHeight > a.clientHeight))
      ) {
        loose = true;
      }
    }
    if (pos === 'fixed') cut(0, 0, root.clientWidth, root.clientHeight, 'the viewport');
    else cut(-scrollX, -scrollY, root.scrollWidth - scrollX, root.scrollHeight - scrollY, 'the page');
    const clip = { x: left, y: top, width: right - left, height: bottom - top };
    const empty = (w, full) => w < 0 || (full > 0 && w <= 0);
    if (empty(clip.width, box.width) || empty(clip.height, box.height)) {
      return { verdict: 'indeterminate', why: ['not drawn: clipped away by ' + by.join(', ')], box, clip, by, warped, loose };
    }
    return { verdict: 'pass', why: [], box, clip, by, warped, loose };
  };

  // seen says whether a person can see what is drawn in box r, styled by
  // e and clipped from first up (pos its position): 'shown', 'hidden' or
  // 'unsure', and why it is not shown.
  const seen = (r, e, own, first, pos) => {
    // checkVisibility: no box, or inside content the browser skips (a
    // closed details, content-visibility hidden), which still measures.
    if (r.width <= 0 || r.height <= 0 || (e.checkVisibility && !e.checkVisibility())) return ['hidden', 'not rendered'];
    const vis = getComputedStyle(e).visibility;
    if (vis !== 'visible') return ['hidden', 'visibility ' + vis];
    const op = effectiveOpacity(e);
    if (op <= VISIBLE_MIN_OPACITY) return ['hidden', 'opacity ' + op.toFixed(2) + ' <= ' + VISIBLE_MIN_OPACITY];
    const m = measure(r, own, first, pos, true);
    if (!nothing(m)) return ['shown', ''];
    const what =
      m.verdict === 'pass' ? 'drawn only ' + m.clip.width.toFixed(1) + 'x' + m.clip.height.toFixed(1) + ' px' : 'clipped away';
    const where = what + (m.by.length ? ' by ' + m.by.join(', ') : '');
    if (m.warped.length) return ['unsure', where + ' under ' + m.warped.join(', ')];
    if (m.loose) return ['unsure', where + ', which scrolls to more'];
    return ['hidden', where];
  };

  // textBox is the box of text node n's own boxes, each line's: zero when
  // it is not rendered.
  const textBox = (n) => {
    const range = document.createRange();
    range.selectNodeContents(n);
    let l = Infinity;
    let t = Infinity;
    let rr = -Infinity;
    let b = -Infinity;
    for (const c of range.getClientRects()) {
      if (c.width <= 0 || c.height <= 0) continue;
      l = Math.min(l, c.left);
      t = Math.min(t, c.top);
      rr = Math.max(rr, c.right);
      b = Math.max(b, c.bottom);
    }
    if (l === Infinity) return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 };
    return { left: l, top: t, right: rr, bottom: b, width: rr - l, height: b - t };
  };

  // textSeen reads el's text as the runner's text expectation does (text
  // nodes in tree order, a shadow root's after its host's own, script,
  // noscript and style skipped, a button input by its value) and sorts it
  // by what a person can see. The text of a select or textarea is seen
  // with its control. Whitespace is nobody's.
  const UNSEEN_MAX = 3;
  const textSeen = () => {
    const out = { verdict: 'pass', why: [], full: '', shown: '', maybe: '', hidden: [], unsure: [] };
    const put = (text, state, who, why) => {
      out.full += text;
      if (state === 'shown') out.shown += text;
      if (state !== 'hidden') out.maybe += text;
      const list = state === 'hidden' ? out.hidden : state === 'unsure' ? out.unsure : null;
      const line = who + ' ' + why;
      if (list && list.length < UNSEEN_MAX && !list.includes(line)) list.push(line);
    };
    const boxed = (text, e) => {
      const [state, why] = seen(e.getBoundingClientRect(), e, e, up(e), getComputedStyle(e).position);
      put(text, state, desc(e), why);
    };
    const walk = (root) => {
      if (root.nodeType === 1 && /^(SCRIPT|NOSCRIPT|STYLE)$/.test(root.nodeName)) return;
      if (root.nodeType === 1 && root.tagName === 'INPUT' && /^(submit|button|reset)$/.test(root.type)) {
        boxed(root.value, root);
        return;
      }
      for (let c = root.firstChild; c; c = c.nextSibling) {
        if (c.nodeType === 1) walk(c);
        if (c.nodeType !== 3) continue;
        const text = c.nodeValue || '';
        const e = c.parentElement || (c.parentNode && c.parentNode.host);
        if (!text.trim() || !e) {
          put(text, 'shown', '', '');
          continue;
        }
        const control = e.closest('select, textarea');
        if (control) {
          boxed(text, control);
          continue;
        }
        const [state, why] = seen(textBox(c), e, null, e, 'static');
        put(text, state, desc(e), why);
      }
      if (root.shadowRoot) walk(root.shadowRoot);
    };
    walk(el);
    return out;
  };

  // contentShows reports whether anything in el shows though el's own box
  // is zero: text of its own or a descendant's, or a descendant's box.
  const contentShows = () => {
    const t = textSeen();
    if (t.maybe.trim()) return true;
    for (const d of el.querySelectorAll('*')) if (shown(d)) return true;
    return false;
  };

  switch (kind) {
    case 'visible':
      return visible();
    case 'legible':
      return legible();
    case 'text':
      return textSeen();
    case 'hit':
      return hit();
    case 'drawn':
      return drawn(el);
    default:
      return { verdict: 'indeterminate', why: ['unknown page function ' + kind] };
  }
}
