// Artifact markdown, rendered with marked and sanitized with DOMPurify
// before it reaches the DOM. Raw HTML in artifacts never runs.
import { useMemo } from "react";
import { marked } from "marked";
import DOMPurify from "dompurify";

export function renderMarkdown(src: string): string {
  const html = marked.parse(src, { async: false, gfm: true }) as string;
  return DOMPurify.sanitize(html, { USE_PROFILES: { html: true }, FORBID_ATTR: ["style"], FORBID_TAGS: ["style", "form", "input"] });
}

export function Markdown({ source }: { source: string }) {
  const html = useMemo(() => renderMarkdown(source), [source]);
  return <div className="markdown" dangerouslySetInnerHTML={{ __html: html }} />;
}
