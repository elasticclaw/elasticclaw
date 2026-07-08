"use client"

import { useCallback, useEffect, useState } from "react"
import { getHubUrl } from "@/lib/hub-url"
import { getAuthToken } from "@/lib/auth-storage"
import type { SettingsData } from "./types"

async function fetchSettings(): Promise<SettingsData> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!res.ok) throw new Error("Failed to load settings")
  return res.json()
}

async function patchSettings(patch: object): Promise<void> {
  const hubUrl = getHubUrl()
  const token = getAuthToken() || ""
  const res = await fetch(`${hubUrl}/api/settings`, {
    method: "PATCH",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  })
  if (!res.ok) throw new Error(await res.text())
}

/**
 * Owns the global settings blob (GET/PATCH /api/settings) shared by the
 * settings sections: loading, saving, and the transient error/success banners.
 */
export function useSettingsData() {
  const [settings, setSettings] = useState<SettingsData | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState("")
  const [success, setSuccess] = useState("")

  const load = useCallback(async () => {
    try {
      setSettings(await fetchSettings())
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load")
    }
  }, [])

  useEffect(() => { load() }, [load])

  const save = useCallback(async (patch: object): Promise<boolean> => {
    setSaving(true)
    setError("")
    setSuccess("")
    try {
      await patchSettings(patch)
      setSuccess("Saved")
      await load()
      setTimeout(() => setSuccess(""), 2000)
      return true
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed")
      return false
    } finally {
      setSaving(false)
    }
  }, [load])

  // Silent save: patches without the global 'Saved' banner (used for toggle-style updates)
  const saveSilent = useCallback(async (patch: object) => {
    try {
      await patchSettings(patch)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : "Save failed")
    }
  }, [load])

  return { settings, saving, error, success, load, save, saveSilent }
}
