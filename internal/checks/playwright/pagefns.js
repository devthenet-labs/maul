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
//            overflow, clip or clip-path inset is a fail, under a scaled,
//            rotated or zoomed ancestor, or another clip-path shape,
//            indeterminate. Covered at its centre by an opaque layer that
//            is not the element or inside it: indeterminate, never a pass.
//            Its own non-emoji text needs a contrast of at least 1.5:1
//            against what is painted under it; with none, the text of its
//            shown descendants is judged so: one passing passes, all
//            failing fails, otherwise indeterminate.
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

  // stackAt is the hit-test stack at (x, y), top first, with pointer-events
  // lifted for the one measurement (a layer with pointer-events none still
  // paints).
  const stackAt = (x, y) => {
    const st = document.createElement('style');
    st.textContent = '*{pointer-events:auto!important}';
    document.documentElement.appendChild(st);
    const stack = document.elementsFromPoint(x, y).filter((n) => n !== st);
    st.remove();
    return stack;
  };

  // layersBelow lists the layers painted under t, top first: the hit-test
  // stack at its centre, else the ancestor chain.
  const layersBelow = (t) => {
    const r = t.getBoundingClientRect();
    const stack = stackAt(r.left + r.width / 2, r.top + r.height / 2);
    const i = stack.indexOf(t);
    if (i >= 0) return stack.slice(i);
    const chain = [];
    for (let n = t; n && n.nodeType === 1; n = n.parentElement) chain.push(n);
    return chain;
  };

  // resolveBackground returns the opaque background candidates under t,
  // or why they cannot be known. background-color paints below
  // background-image; a gradient contributes each stop as a candidate.
  const resolveBackground = (t) => {
    const layers = layersBelow(t);
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
    for (let n = e; n && n.nodeType === 1; n = n.parentElement) o *= parseFloat(getComputedStyle(n).opacity);
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
    for (let n = t; n && n.nodeType === 1; n = n.parentElement) {
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

  // paints says what n paints over its whole box, or '' for nothing a
  // person would see: faded out, or no background, image or replaced
  // content (text alone, a floating label, is not a cover).
  const REPLACED = /^(IMG|CANVAS|VIDEO|OBJECT|EMBED|IFRAME)$/;
  const paints = (n) => {
    if (effectiveOpacity(n) <= VISIBLE_MIN_OPACITY) return '';
    if (REPLACED.test(n.tagName) || n instanceof SVGElement) return desc(n);
    const s = getComputedStyle(n);
    const bg = parseColour(s.backgroundColor);
    if (!bg || bg.a > 0) return 'the background of ' + desc(n);
    if (s.backgroundImage !== 'none') return 'the background image of ' + desc(n);
    if (s.backdropFilter && s.backdropFilter !== 'none') return 'the backdrop-filter of ' + desc(n);
    return '';
  };

  // covered says what covers el where a person would look for it: at the
  // centre of its box, or, when that point misses it (a link wrapped over
  // two lines), of the first of its line boxes it is found at. Its own
  // descendants are its content, never a cover; '' when nothing does.
  const covered = () => {
    const rs = [el.getBoundingClientRect(), ...el.getClientRects()];
    for (const r of rs) {
      if (r.width <= 0 || r.height <= 0) continue;
      const x = r.left + r.width / 2;
      const y = r.top + r.height / 2;
      if (!(x >= 0 && x < innerWidth && y >= 0 && y < innerHeight)) continue;
      const stack = stackAt(x, y);
      const i = stack.indexOf(el);
      if (i < 0) continue;
      for (const n of stack.slice(0, i)) {
        if (el.contains(n)) continue;
        const what = paints(n);
        if (what) return 'centre (' + Math.round(x) + ',' + Math.round(y) + ') covered by ' + what;
      }
      return '';
    }
    return 'not found at its centre';
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
  // indeterminate. With no text at all, the element is seen by its box.
  const DESCENDANTS_MAX = 50;
  const descendantText = () => {
    const verdicts = [];
    let more = false;
    for (const d of el.querySelectorAll('*')) {
      if (!ownText(d) || !shown(d)) continue;
      if (verdicts.length === DESCENDANTS_MAX) {
        more = true;
        break;
      }
      const v = judge(contrast(d), VISIBLE_MIN_CONTRAST);
      if (v.verdict === 'pass') return v;
      verdicts.push({ ...v, why: v.why.map((w) => desc(d) + ': ' + w) });
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
    if (r.width <= 0 || r.height <= 0) why.push('zero box ' + r.width + 'x' + r.height);
    const cx = r.left + r.width / 2;
    const cy = r.top + r.height / 2;
    if (!(cx >= 0 && cx < innerWidth && cy >= 0 && cy < innerHeight)) {
      why.push('centre (' + Math.round(cx) + ',' + Math.round(cy) + ') outside the viewport');
    }
    const vis = getComputedStyle(el).visibility;
    if (vis !== 'visible') why.push('visibility ' + vis);
    const op = effectiveOpacity(el);
    if (op <= VISIBLE_MIN_OPACITY) why.push('opacity ' + op.toFixed(2) + ' <= ' + VISIBLE_MIN_OPACITY);
    if (why.length) return { verdict: 'fail', why };
    const m = drawn(el);
    if (nothing(m)) {
      const what =
        m.verdict === 'pass'
          ? 'drawn only ' + m.clip.width.toFixed(1) + 'x' + m.clip.height.toFixed(1) + ' px'
          : 'clipped away';
      const where = what + (m.by.length ? ' by ' + m.by.join(', ') : '');
      if (m.warped.length) return { verdict: 'indeterminate', why: [where + ' under ' + m.warped.join(', ')] };
      return { verdict: 'fail', why: ['not drawn: ' + where] };
    }
    if (m.shaped.length) return { verdict: 'indeterminate', why: ['clipped by a shape: ' + m.shaped.join(', ')] };
    const cover = covered();
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
    const top = document.elementFromPoint(x, y);
    if (top && (top === el || el.contains(top))) return { verdict: 'pass', why: [] };
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
  // neither clips; shape names a clip-path the arithmetic cannot model.
  const clipOf = (n, s) => {
    const r = n.getBoundingClientRect();
    let l = -Infinity;
    let t = -Infinity;
    let rr = Infinity;
    let b = Infinity;
    let clips = false;
    let shape = '';
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
      } else shape = 'clip-path ' + s.clipPath + ' on ' + desc(n);
    }
    if (![l, t, rr, b].every((x) => !Number.isNaN(x))) return { shape: 'clip ' + s.clip + ' on ' + desc(n) };
    return { box: clips ? { l, t, r: rr, b } : null, shape };
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
  // clip and clip-path inset() cut too, t's own and its clipping chain's;
  // another clip-path shape is named in shaped, and an ancestor that
  // scales, rotates or zooms in warped, since its cut is then unsure.
  const drawn = (t) => {
    const r = t.getBoundingClientRect();
    const box = { x: r.left, y: r.top, width: r.width, height: r.height };
    let left = r.left;
    let top = r.top;
    let right = r.right;
    let bottom = r.bottom;
    const by = [];
    const cut = (l, t, rr, b, who) => {
      if (l > left || t > top || rr < right || b < bottom) by.push(who);
      left = Math.max(left, l);
      top = Math.max(top, t);
      right = Math.min(right, rr);
      bottom = Math.min(bottom, b);
    };
    const shaped = [];
    const warped = [];
    const clipBy = (n, s) => {
      const c = clipOf(n, s);
      if (c.shape) shaped.push(c.shape);
      if (c.box) cut(c.box.l, c.box.t, c.box.r, c.box.b, desc(n));
    };
    const root = document.documentElement;
    const own = getComputedStyle(t);
    clipBy(t, own);
    for (let a = t.parentElement; a && a !== root; a = a.parentElement) if (warps(getComputedStyle(a))) warped.push(desc(a));
    let pos = own.position;
    for (let a = t.parentElement; a && a !== root && a !== document.body; a = a.parentElement) {
      const s = getComputedStyle(a);
      if (pos === 'fixed' && !establishesFixedBlock(s)) continue;
      if (pos === 'absolute' && s.position === 'static' && !establishesFixedBlock(s)) continue;
      pos = s.position;
      clipBy(a, s);
      const clipX = s.overflowX !== 'visible';
      const clipY = s.overflowY !== 'visible';
      if ((!clipX && !clipY) || /^(inline|contents)$/.test(s.display)) continue;
      const ar = a.getBoundingClientRect();
      const l = ar.left + a.clientLeft;
      const t = ar.top + a.clientTop;
      const w = clipX ? a.clientWidth : Infinity;
      const h = clipY ? a.clientHeight : Infinity;
      cut(clipX ? l : -Infinity, clipY ? t : -Infinity, l + w, t + h, desc(a));
    }
    if (pos === 'fixed') cut(0, 0, root.clientWidth, root.clientHeight, 'the viewport');
    else cut(-scrollX, -scrollY, root.scrollWidth - scrollX, root.scrollHeight - scrollY, 'the page');
    const clip = { x: left, y: top, width: right - left, height: bottom - top };
    const empty = (w, full) => w < 0 || (full > 0 && w <= 0);
    if (empty(clip.width, box.width) || empty(clip.height, box.height)) {
      return { verdict: 'indeterminate', why: ['not drawn: clipped away by ' + by.join(', ')], box, clip, by, shaped, warped };
    }
    return { verdict: 'pass', why: [], box, clip, by, shaped, warped };
  };

  switch (kind) {
    case 'visible':
      return visible();
    case 'legible':
      return legible();
    case 'hit':
      return hit();
    case 'drawn':
      return drawn(el);
    default:
      return { verdict: 'indeterminate', why: ['unknown page function ' + kind] };
  }
}
