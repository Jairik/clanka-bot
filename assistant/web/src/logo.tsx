import type { SVGProps } from "react";
import { useId } from "react";

// The clanka mark: an open ring face in pixel "deal with it" shades, with the
// red record dot in the ring's gap. Source and app icons: docs/brand/.
//
// Inline rather than <img> so it inherits currentColor like the lucide icons
// it replaced, and the dot follows the theme's --danger token. The shades cut
// the ring through a mask; useId keeps that mask's id unique per instance.
const SHADES = "M194,420h576v32h-576ZM162,452h64v32h-64ZM290,452h32v32h-32ZM354,452h32v32h-32ZM418,452h96v32h-96ZM546,452h32v32h-32ZM610,452h32v32h-32ZM674,452h96v32h-96ZM290,484h64v32h-64ZM386,484h32v32h-32ZM450,484h64v32h-64ZM546,484h64v32h-64ZM642,484h32v32h-32ZM706,484h64v32h-64ZM322,516h160v32h-160ZM578,516h160v32h-160ZM354,548h96v32h-96ZM610,548h96v32h-96Z";
const SHADES_KNOCKOUT = SHADES + "M322,452h32v32h-32ZM386,452h32v32h-32ZM578,452h32v32h-32ZM642,452h32v32h-32ZM354,484h32v32h-32ZM418,484h32v32h-32ZM610,484h32v32h-32ZM674,484h32v32h-32Z";

export function Logo({ className, ...rest }: SVGProps<SVGSVGElement>) {
  const mask = `logo-ko-${useId()}`;
  return (
    <svg viewBox="150 150 724 724" className={className} aria-hidden="true" focusable="false" {...rest}>
      <defs>
        <mask id={mask} maskUnits="userSpaceOnUse" x="0" y="0" width="1024" height="1024">
          <rect width="1024" height="1024" fill="#fff" />
          <path d={SHADES_KNOCKOUT} fill="#000" stroke="#000" strokeWidth={44} strokeLinejoin="miter" transform="rotate(15 512 512)" />
        </mask>
      </defs>
      <path d="M746.8,561.9A240,240 0 1 1 532.9,272.9" fill="none" stroke="currentColor" strokeWidth={76} strokeLinecap="round" mask={`url(#${mask})`} />
      <path d={SHADES} fill="currentColor" transform="rotate(15 512 512)" shapeRendering="crispEdges" />
      <circle cx="681.7" cy="342.3" r="46" className="fill-danger" />
    </svg>
  );
}
