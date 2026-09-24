export const chatMarkdownOptions = {
  theme: 'dracula-soft',
  defaultHighlightLang: 'javascript',
  html: false,
  attrs: { disable: true }
};

// Only server-supplied source identities can resolve citation links.
export function sourceCitations(value: unknown): Api.Chat.SourceCitation[] {
  if (!Array.isArray(value) || value.length > 24) return [];
  const seen = new Set<number>();
  const sources: Api.Chat.SourceCitation[] = [];
  for (const source of value) {
    if (!source || !Number.isInteger(source.number) || source.number < 1 || source.number > 24 ||
      !Number.isSafeInteger(source.documentId) || source.documentId <= 0 ||
      typeof source.version !== 'string' || !source.version.trim() || source.version.length > 64 ||
      typeof source.fileName !== 'string' || seen.has(source.number)) return [];
    seen.add(source.number);
    sources.push(source);
  }
  return sources;
}

function citationLabel(text: string): string {
  // Entities are parsed as text, so neither HTML nor Markdown in filenames can create elements.
  return Array.from(text, char => `&#${char.codePointAt(0)};`).join('');
}

export function renderSourceCitations(text: string, sources: Api.Chat.SourceCitation[]): string {
  const byNumber = new Map(sourceCitations(sources).map(source => [source.number, source]));
  return text.replace(/\[来源#(\d+)\]/g, (marker, number) => {
    const source = byNumber.get(Number(number));
    if (!source) return marker;
    return `[来源#${source.number}: ${citationLabel(source.fileName)}](#source-${source.number})`;
  });
}
