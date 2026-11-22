// The world map without a map library: Natural Earth 110m country
// outlines (public domain; src/map/countries-110m.json, a TopoJSON keyed by
// ISO 3166-1 alpha-2, Antarctica left out) decoded and projected by hand,
// equirectangular, as in the mock. Loaded as its own chunk on first use.

import { useEffect, useRef, useState } from "react";
import type { ListRow } from "./api";
import { LEVEL } from "./charts";
import { countryName, num } from "./format";
import { useWidth } from "./hooks";
import { Tip, useTip } from "./ui";

type Ring = number[];
export type Topology = {
  transform: { scale: [number, number]; translate: [number, number] };
  objects: {
    countries: {
      geometries: {
        type: "Polygon" | "MultiPolygon";
        arcs: Ring[] | Ring[][];
        id: string;
      }[];
    };
  };
  arcs: [number, number][][];
};

export type Country = { id: string; polygons: [number, number][][][] };

// Delta-decoded arcs stitched into rings, lon/lat.
export function decode(topo: Topology): Country[] {
  const [sx, sy] = topo.transform.scale;
  const [tx, ty] = topo.transform.translate;
  const arcs = topo.arcs.map((a) => {
    let x = 0;
    let y = 0;
    return a.map(([dx, dy]): [number, number] => [
      (x += dx) * sx + tx,
      (y += dy) * sy + ty,
    ]);
  });
  const ring = (r: Ring): [number, number][] => {
    let pts: [number, number][] = [];
    for (const i of r) {
      const a = i < 0 ? arcs[~i].slice().reverse() : arcs[i];
      pts = pts.concat(pts.length ? a.slice(1) : a);
    }
    // A ring across the antimeridian (Russia, Fiji) is drawn east of it.
    const lons = pts.map((p) => p[0]);
    if (Math.max(...lons) - Math.min(...lons) > 180)
      pts = pts.map(([lon, lat]) => [lon < 0 ? lon + 360 : lon, lat]);
    return pts;
  };
  return topo.objects.countries.geometries.map((g) => {
    const polys = (g.type === "Polygon" ? [g.arcs] : g.arcs) as Ring[][];
    return { id: g.id, polygons: polys.map((p) => p.map(ring)) };
  });
}

export function pathOf(c: Country, W: number, H: number): string {
  const px = ([lon, lat]: [number, number]) =>
    `${(((lon + 180) / 360) * W).toFixed(1)} ${(((84 - lat) / 142) * H).toFixed(1)}`;
  return c.polygons
    .map((p) => p.map((r) => `M${r.map(px).join("L")}Z`).join(""))
    .join("");
}

let cache: Country[] | null = null;

function useCountries(): { countries: Country[] | null; failed: boolean } {
  const [state, setState] = useState<{ c: Country[] | null; failed: boolean }>({
    c: cache,
    failed: false,
  });
  useEffect(() => {
    if (cache) return;
    let live = true;
    import("./map/countries-110m.json").then(
      (m) => {
        cache = decode(m.default as unknown as Topology);
        if (live) setState({ c: cache, failed: false });
      },
      () => live && setState({ c: null, failed: true }),
    );
    return () => {
      live = false;
    };
  }, []);
  return { countries: state.c, failed: state.failed };
}

// Countries shaded in the backend's four steps (row.level, 1 to 4). Until
// the backend sends levels, a country with data takes the second step.
export function WorldMap({
  rows,
  metric,
}: {
  rows: ListRow[];
  metric: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const W = Math.max(320, useWidth(ref, 640));
  const H = Math.round(W * 0.47);
  const { countries, failed } = useCountries();
  const { tip, show, hide } = useTip();
  const byCc = new Map(rows.map((r) => [r.key.toUpperCase(), r]));
  return (
    <div ref={ref} className="map">
      {failed ? (
        <p className="m-0 py-6 text-[13px] text-muted">
          The country outlines could not be loaded.
        </p>
      ) : !countries ? (
        <div className="skeleton" style={{ height: H }} aria-busy="true" />
      ) : (
        <svg
          width={W}
          height={H}
          viewBox={`0 0 ${W} ${H}`}
          role="img"
          aria-label={`${metric} by country`}
          className="block max-w-full"
        >
          {countries.map((c, i) => {
            const r = byCc.get(c.id);
            const level = r ? Math.max(1, Math.min(4, r.level ?? 2)) : 0;
            return (
              <path
                key={i}
                data-cc={c.id}
                className="chart-fill"
                style={{ animationDelay: r ? "300ms" : "0ms" }}
                d={pathOf(c, W, H)}
                fill={LEVEL[level]}
                onMouseMove={(e) =>
                  show(
                    e,
                    r?.label && r.label !== r.key ? r.label : countryName(c.id),
                    [[metric, r ? num(r.value) : "none"]],
                  )
                }
                onMouseLeave={hide}
              />
            );
          })}
        </svg>
      )}
      <Tip tip={tip} />
    </div>
  );
}
