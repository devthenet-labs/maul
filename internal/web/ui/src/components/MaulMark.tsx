// The maul mark — a splitting maul (axe blade, hammer face, ash handle) with a
// green cutting edge, drawn as the docs logo (docs/assets/images/logo.svg) is.

export function MaulMark({ size = 30 }: { size?: number }) {
  return (
    <svg class="maul-mark" width={size} height={size} viewBox="0 0 128 128" aria-hidden="true">
      <rect x="5" y="5" width="118" height="118" rx="26" fill="#151a19" stroke="var(--maul-line-2)" stroke-width="3" />
      <g transform="translate(67 63) rotate(-34) scale(0.74) translate(-62 -66)">
        <rect x="58" y="44" width="12" height="86" rx="5.5" fill="#c99a68" />
        <path d="M42 26 L24 16 Q15 39 24 62 L42 52 Z" fill="#c3ccc8" />
        <rect x="40" y="25" width="50" height="28" rx="4" fill="#c3ccc8" />
        <rect x="88" y="27" width="10" height="24" rx="2.5" fill="#7f8b87" />
        <path d="M24 16 Q15 39 24 62" fill="none" stroke="#86e05a" stroke-width="5" stroke-linecap="round" />
      </g>
    </svg>
  );
}
