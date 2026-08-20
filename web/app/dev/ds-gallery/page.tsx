import { notFound } from "next/navigation"
import { DesignSystemGalleryPage } from "@/components/dev/ds-gallery"

export default function DevDesignSystemGalleryRoute() {
  if (process.env.NODE_ENV === "production") notFound()
  return <DesignSystemGalleryPage />
}
