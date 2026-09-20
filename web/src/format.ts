// Byte formatting for card sizes and the footer total, e.g. "8.4 GB".
// Units are decimal (1 GB = 1,000,000,000 bytes), matching the sizes YouTube
// and iOS display.

export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit++;
  }
  const decimals = unit === 0 || value >= 100 ? 0 : 1;
  return `${value.toFixed(decimals)} ${units[unit]}`;
}
