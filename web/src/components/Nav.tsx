import { useTranslation } from "react-i18next";
import { SITE } from "../config";
import { LanguageSwitcher } from "./LanguageSwitcher";

// Sticky glassmorphism navigation bar.
export function Nav() {
  const { t } = useTranslation();

  return (
    <nav className="nav">
      <div className="container nav-inner">
        <a className="nav-brand" href="#top">
          <span className="nav-brand-mark" aria-hidden="true">
            🦆
          </span>
          <span>flock</span>
        </a>

        <div className="nav-links">
          <a href="#how">{t("flock.nav.how")}</a>
          <a href="#features">{t("flock.nav.features")}</a>
          <a href="#deploy">{t("flock.nav.deploy")}</a>
          <a href={SITE.docsUrl} target="_blank" rel="noreferrer">
            {t("flock.nav.docs")}
          </a>
          <a href={SITE.githubUrl} target="_blank" rel="noreferrer">
            {t("flock.nav.github")}
          </a>
        </div>

        <div className="nav-actions">
          <LanguageSwitcher />
          <a className="btn btn-primary" href="#deploy">
            {t("flock.nav.cta")}
          </a>
        </div>
      </div>
    </nav>
  );
}
