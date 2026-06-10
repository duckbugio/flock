import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { HelmetProvider } from "react-helmet-async";
import { I18nextProvider } from "react-i18next";
import i18n from "../i18n";

// renderWithProviders mounts a component inside the configured i18n instance and
// a HelmetProvider so components that read translations or emit <head> tags work.
export function renderWithProviders(ui: ReactElement) {
  return render(
    <HelmetProvider>
      <I18nextProvider i18n={i18n}>{ui}</I18nextProvider>
    </HelmetProvider>,
  );
}
