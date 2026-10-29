// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT
//
// maul's page functions (SPEC Q9, spike SP-5): one function the support
// module runs on an element through locator.evaluate, so nothing is
// installed in the page and no page script can stand in for it. It
// returns {verdict: "pass" | "fail" | "indeterminate", why: [...]}; an
// indeterminate verdict is a check error (unsure), never a fail.
//
//   visible: a non-zero box whose centre is in the viewport (the caller
//            scrolls it in first), visibility visible, effective opacity
//            above 0.1 and, with its own non-emoji text, contrast of at
//            least 1.5:1 against what is painted under it.
//   legible: WCAG AA contrast of its own non-emoji text, 4.5:1, or 3:1
//            for large text (24px, or 18.66px at weight 700 and over).
//   hit:     the topmost element at the box centre is the element or
//            inside it (clickable's hit test; a pass is confirmed by the
//            caller's trial click).
//
// The background is resolved from what is painted under the element: the
// hit-test stack at its centre with pointer-events lifted, the ancestor
// chain only when the element is not in that stack. A gradient is judged
// against each of its colour stops composited over what lies beneath, by
// unanimity: every stop passes, pass; every stop fails, fail; otherwise
// indeterminate. A background image is indeterminate. Paint effects the
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

  // layersBelow lists the layers painted under el, top first: the hit-test
  // stack at its centre, with pointer-events lifted for the one
  // measurement (a layer with pointer-events none still paints), else the
  // ancestor chain.
  const layersBelow = () => {
    const r = el.getBoundingClientRect();
    const st = document.createElement('style');
    st.textContent = '*{pointer-events:auto!important}';
    document.documentElement.appendChild(st);
    const stack = document.elementsFromPoint(r.left + r.width / 2, r.top + r.height / 2).filter((n) => n !== st);
    st.remove();
    const i = stack.indexOf(el);
    if (i >= 0) return stack.slice(i);
    const chain = [];
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) chain.push(n);
    return chain;
  };

  // resolveBackground returns the opaque background candidates under el,
  // or why they cannot be known. background-color paints below
  // background-image; a gradient contributes each stop as a candidate.
  const resolveBackground = () => {
    const layers = layersBelow();
    const walk = (k) => {
      if (k >= layers.length) return { colours: [canvasColour()] };
      const node = layers[k];
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

  // ownText is el's own text, colour emoji left out (they ignore the CSS
  // colour); a form control's value counts as its own text.
  const EMOJI = /[\p{Emoji_Presentation}\u{FE0F}\u{200D}\u{20E3}\u{1F3FB}-\u{1F3FF}]/gu;
  const ownText = () => {
    let t = '';
    for (const n of el.childNodes) if (n.nodeType === 3) t += n.data;
    if (
      (el.tagName === 'INPUT' && !/^(checkbox|radio|range|color|file|hidden|image)$/.test(el.type)) ||
      el.tagName === 'TEXTAREA'
    ) {
      t += el.value || el.placeholder || '';
    }
    if (el.tagName === 'SELECT' && el.selectedOptions[0]) t += el.selectedOptions[0].text;
    return t.trim().replace(EMOJI, '').trim();
  };

  const effectiveOpacity = () => {
    let o = 1;
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) o *= parseFloat(getComputedStyle(n).opacity);
    return o;
  };

  // contrast returns the contrast range of el's text over its background
  // candidates, or why it cannot be known. The text colour's alpha is
  // multiplied by the effective opacity before compositing.
  const contrast = () => {
    const cs = getComputedStyle(el);
    const fg = parseColour(cs.color);
    if (!fg) return { indeterminate: 'unparsed color ' + cs.color };
    if (cs.webkitTextFillColor && cs.webkitTextFillColor !== cs.color) {
      const f = parseColour(cs.webkitTextFillColor);
      if (f && f.a === 0) return { indeterminate: 'transparent text fill' };
    }
    const effects = [];
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
      const s = getComputedStyle(n);
      if (/text/.test(s.backgroundClip) || /text/.test(s.webkitBackgroundClip || '')) {
        return { indeterminate: 'background-clip text on ' + desc(n) };
      }
      if (n === el && s.textShadow !== 'none') effects.push('text-shadow');
      if (s.mixBlendMode !== 'normal') effects.push('mix-blend-mode on ' + desc(n));
      if (s.filter !== 'none') effects.push('filter on ' + desc(n));
      if (s.backdropFilter && s.backdropFilter !== 'none') effects.push('backdrop-filter on ' + desc(n));
    }
    const bg = resolveBackground();
    if (bg.indeterminate) return bg;
    const fgA = { ...fg, a: fg.a * effectiveOpacity() };
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

  const visible = () => {
    const r = el.getBoundingClientRect();
    const why = [];
    if (r.width <= 0 || r.height <= 0) why.push('zero box ' + r.width + 'x' + r.height);
    const cx = r.left + r.width / 2;
    const cy = r.top + r.height / 2;
    if (!(cx >= 0 && cx < innerWidth && cy >= 0 && cy < innerHeight)) {
      why.push('centre (' + Math.round(cx) + ',' + Math.round(cy) + ') outside the viewport');
    }
    const vis = getComputedStyle(el).visibility;
    if (vis !== 'visible') why.push('visibility ' + vis);
    const op = effectiveOpacity();
    if (op <= VISIBLE_MIN_OPACITY) why.push('opacity ' + op.toFixed(2) + ' <= ' + VISIBLE_MIN_OPACITY);
    if (why.length) return { verdict: 'fail', why };
    if (!ownText()) return { verdict: 'pass', why: [] };
    return judge(contrast(), VISIBLE_MIN_CONTRAST);
  };

  const legible = () => {
    if (!ownText()) return { verdict: 'indeterminate', why: ['no own text (colour emoji are not text)'] };
    const c = contrast();
    return judge(c, c.large ? LEGIBLE_MIN_CONTRAST_LARGE : LEGIBLE_MIN_CONTRAST);
  };

  const hit = () => {
    const r = el.getBoundingClientRect();
    const x = r.left + r.width / 2;
    const y = r.top + r.height / 2;
    const top = document.elementFromPoint(x, y);
    if (top && (top === el || el.contains(top))) return { verdict: 'pass', why: [] };
    return {
      verdict: 'fail',
      why: ['centre (' + Math.round(x) + ',' + Math.round(y) + ') hits ' + (top ? desc(top) : 'nothing')],
    };
  };

  switch (kind) {
    case 'visible':
      return visible();
    case 'legible':
      return legible();
    case 'hit':
      return hit();
    default:
      return { verdict: 'indeterminate', why: ['unknown page function ' + kind] };
  }
}
