import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";

interface CodeBlockProps {
  code: string;
}

// CodeBlock renders a monospace snippet with a copy-to-clipboard button.
export function CodeBlock({ code }: CodeBlockProps) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const onCopy = useCallback(() => {
    const done = () => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    };
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(code).then(done, () => {
        /* clipboard denied — leave button label unchanged */
      });
    }
  }, [code]);

  return (
    <div className="code-block">
      <button type="button" className="code-copy" onClick={onCopy}>
        {copied ? t("flock.quickstart.copied") : t("flock.quickstart.copy")}
      </button>
      <pre>
        <code>{code}</code>
      </pre>
    </div>
  );
}
