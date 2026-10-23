import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderAt } from "../test/render";
import { mockFetch } from "../test/mocks";

const m = { id: "m1", hostname: "box-one", os: "linux", arch: "amd64", gtb_version: "0.1.0", protocol: "1", created_at: "2026-10-01T00:00:00Z", last_seen_at: "2026-10-22T10:00:00Z", revoked_at: null, freshness_seconds: 3, stale: false, rigs: [{ name: "core", remote: "", gated: true }] };

describe("Machines", () => {
  it("revokes only after the in-page confirmation", async () => {
    const calls = mockFetch({
      "GET /api/v1/me": { body: { login: "admin", csrf_token: "tok" } },
      "GET /api/v1/machines": { body: [m] },
      "DELETE /api/v1/machines/m1": { body: { ok: true } },
    });
    renderAt("/machines");
    expect(await screen.findByText("box-one")).toBeInTheDocument();
    expect(screen.getByText("linux/amd64")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Revoke box-one" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Confirm revoke box-one" }));
    await waitFor(() => expect(calls.find((c) => c.method === "DELETE")?.headers.get("X-CSRF-Token")).toBe("tok"));
  });
});
