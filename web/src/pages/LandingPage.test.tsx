import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import i18n from "../i18n";
import { renderWithProviders } from "../test/render";
import { LandingPage } from "./LandingPage";

describe("LandingPage", () => {
  beforeEach(async () => {
    await i18n.changeLanguage("en");
  });

  it("renders the landing with the hero heading and key sections", () => {
    renderWithProviders(<LandingPage />);

    // Hero title (EN, verbatim from the content spec).
    expect(
      screen.getByRole("heading", {
        level: 1,
        name: "An AI dev team you drive from Telegram",
      }),
    ).toBeInTheDocument();

    // The five pipeline roles are all present.
    expect(screen.getByText("Planner")).toBeInTheDocument();
    expect(screen.getByText("Coder")).toBeInTheDocument();
    expect(screen.getByText("Tester")).toBeInTheDocument();
    expect(screen.getByText("Reviewer")).toBeInTheDocument();
    expect(screen.getByText("Arbiter")).toBeInTheDocument();
  });

  it("shows a translated English string from the FAQ", () => {
    renderWithProviders(<LandingPage />);

    expect(screen.getByText("Is it really free?")).toBeInTheDocument();
  });

  it("links the managed card to roost", () => {
    renderWithProviders(<LandingPage />);

    const roostLinks = screen
      .getAllByRole("link")
      .filter((a) => a.getAttribute("href") === "https://roost.duckbug.io");
    expect(roostLinks.length).toBeGreaterThan(0);
  });

  it("switches the rendered copy when the language changes", async () => {
    const user = userEvent.setup();
    renderWithProviders(<LandingPage />);

    // Start in English.
    expect(
      screen.getByText("An AI dev team you drive from Telegram"),
    ).toBeInTheDocument();

    const switcher = screen.getByLabelText("Language");
    await user.selectOptions(switcher, "ru");

    // Russian copy replaces the English copy.
    await waitFor(() => {
      expect(
        screen.getByText(
          "ИИ-команда разработки, которой вы управляете из Telegram",
        ),
      ).toBeInTheDocument();
    });
    expect(
      screen.queryByText("An AI dev team you drive from Telegram"),
    ).not.toBeInTheDocument();
  });

  it("renders the quickstart copy button", () => {
    renderWithProviders(<LandingPage />);

    const deploy = screen.getByRole("heading", {
      name: "Up in a couple of commands",
    }).parentElement!;
    expect(
      within(deploy).getByRole("button", { name: "Copy" }),
    ).toBeInTheDocument();
  });
});
