interface SkeletonProps {
  width?: string | number;
  height?: string | number;
  radius?: string | number;
  style?: React.CSSProperties;
}

/** A shimmering placeholder block. */
export function Skeleton({ width = '100%', height = 14, radius = 4, style }: SkeletonProps) {
  return (
    <span
      className="skeleton"
      style={{ display: 'block', width, height, borderRadius: radius, ...style }}
    />
  );
}
