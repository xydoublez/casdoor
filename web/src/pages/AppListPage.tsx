import * as React from "react";
import i18next from "i18next";
import {LayoutGrid, Pin, PinOff, Search, X} from "lucide-react";
import {Button} from "@/components/ui/button";
import {Card, CardContent} from "@/components/ui/card";
import {Input} from "@/components/ui/input";
import {Loading} from "@/components/common/Loading";
import {silentSigninLink} from "@/components/common/GridCards";
import {PageHeader} from "@/components/crud/PageHeader";
import {useAccount} from "@/hooks/use-account";
import * as ApplicationBackend from "@/backend/ApplicationBackend";
import {cn} from "@/lib/utils";

/** the antd page's generateTagColor(): a djb2 hash into a fixed palette */
const TAG_COLORS = [
  "#ff4d4f", "#f5222d", "#ff7a45", "#fa541c",
  "#ffa940", "#fa8c16", "#ffc53d", "#faad14",
  "#ffec3d", "#fadb14", "#bae637", "#a0d911",
  "#73d13d", "#52c41a", "#36cfc9", "#13c2c2",
  "#40a9ff", "#1890ff", "#f759ab", "#eb2f96",
];

function tagColor(tag: string) {
  let hash = 5381;
  for (let i = 0; i < tag.length; i++) {
    hash = (hash * 33) ^ tag.charCodeAt(i);
  }
  return TAG_COLORS[Math.abs(hash) % TAG_COLORS.length];
}

type RecentEntry = {name: string; count: number; lastUsed: number};

const MAX_FREQUENT = 8;

export default function AppListPage() {
  const {account} = useAccount();
  const [applications, setApplications] = React.useState<any[] | null>(null);
  const [search, setSearch] = React.useState("");
  const [selectedTags, setSelectedTags] = React.useState<string[]>([]);
  const [recent, setRecent] = React.useState<RecentEntry[]>([]);
  const [pinned, setPinned] = React.useState<string[]>([]);
  const searchRef = React.useRef<HTMLInputElement>(null);

  const recentKey = account ? `casdoor-portal-apps:recent:${account.owner}:${account.name}` : "";
  const pinnedKey = account ? `casdoor-portal-apps:pinned:${account.owner}:${account.name}` : "";

  React.useEffect(() => {
    if (!account) {
      return;
    }
    ApplicationBackend.getApplicationsByOrganization("admin", account.owner).then((res: any) => {
      const items = [...(res.data ?? [])].sort((a: any, b: any) => a.order - b.order);
      setApplications(items);
    });
    try {
      setRecent(JSON.parse(localStorage.getItem(recentKey) ?? "[]"));
      setPinned(JSON.parse(localStorage.getItem(pinnedKey) ?? "[]"));
    } catch {
      // a corrupted store just means "no favorites yet"
    }
  }, [account, recentKey, pinnedKey]);

  // "/" focuses the search box, the way command palettes behave
  React.useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement;
      if (e.key === "/" && !["INPUT", "TEXTAREA"].includes(target.tagName) && !target.isContentEditable) {
        e.preventDefault();
        searchRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  const persistRecent = React.useCallback((entries: RecentEntry[]) => {
    setRecent(entries);
    localStorage.setItem(recentKey, JSON.stringify(entries));
  }, [recentKey]);

  const persistPinned = React.useCallback((names: string[]) => {
    setPinned(names);
    localStorage.setItem(pinnedKey, JSON.stringify(names));
  }, [pinnedKey]);

  const recordOpen = React.useCallback((appName: string) => {
    persistRecent([
      {name: appName, count: (recent.find((entry) => entry.name === appName)?.count ?? 0) + 1, lastUsed: Date.now()},
      ...recent.filter((entry) => entry.name !== appName),
    ].slice(0, 20));
  }, [recent, persistRecent]);

  const togglePin = React.useCallback((appName: string) => {
    persistPinned(pinned.includes(appName) ? pinned.filter((name) => name !== appName) : [...pinned, appName]);
  }, [pinned, persistPinned]);

  if (applications === null) {
    return <Loading />;
  }

  const appByName = new Map(applications.map((application: any) => [application.name, application]));
  const allTags = Array.from(new Set(applications.flatMap((application: any) => (Array.isArray(application.tags) ? application.tags : []))));

  const query = search.trim().toLowerCase();
  const isFiltering = query !== "" || selectedTags.length > 0;

  const filtered = applications.filter((application: any) => {
    if (selectedTags.length > 0 && !(Array.isArray(application.tags) && selectedTags.every((tag) => application.tags.includes(tag)))) {
      return false;
    }
    if (query === "") {
      return true;
    }
    const text = [application.displayName, application.name, application.description, ...(Array.isArray(application.tags) ? application.tags : [])]
      .filter(Boolean)
      .join(" ")
      .toLowerCase();
    return text.includes(query);
  });

  // frequent = pinned first (in pinned order), then most recently opened; stale entries drop out
  const frequent = isFiltering
    ? []
    : [
      ...pinned.map((name) => appByName.get(name)).filter(Boolean),
      ...recent.map((entry) => appByName.get(entry.name)).filter(Boolean),
    ].filter((application, index, self) => self.indexOf(application) === index).slice(0, MAX_FREQUENT) as any[];

  const frequentNames = new Set(frequent.map((application: any) => application.name));
  const openApp = (application: any) => recordOpen(application.name);

  // category grouping: with no search and no tag filter, the full list renders as
  // one section per tag (an app's first tag picks its section), untagged apps last
  const grouped = !isFiltering && allTags.length > 0;
  const untagged = applications.filter((application: any) => !Array.isArray(application.tags) || application.tags.length === 0);
  const sections: {tag: string | null; apps: any[]}[] = grouped
    ? [
      ...allTags
        .map((tag) => ({tag: tag as string | null, apps: applications.filter((application: any) => Array.isArray(application.tags) && application.tags[0] === tag)}))
        .filter((section) => section.apps.length > 0),
      ...(untagged.length > 0 ? [{tag: null, apps: untagged}] : []),
    ]
    : [];

  return (
    <div className="space-y-5">
      <PageHeader
        title={i18next.t("general:Apps")}
        description={i18next.t("general:{{count}} applications", {count: applications.length})}
      />

      {/* toolbar: search + tag chips */}
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center">
        <div className="relative w-full lg:max-w-xs">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            ref={searchRef}
            className="pl-8 pr-16"
            placeholder={i18next.t("general:Search applications")}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => e.key === "Escape" && setSearch("")}
          />
          {search === "" ? (
            <kbd className="pointer-events-none absolute right-2.5 top-1/2 hidden -translate-y-1/2 rounded border bg-muted px-1.5 py-0.5 font-sans text-xs text-muted-foreground sm:block">/</kbd>
          ) : (
            <button
              type="button"
              aria-label={i18next.t("general:Clear search")}
              className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
              onClick={() => setSearch("")}
            >
              <X className="size-3.5" />
            </button>
          )}
        </div>

        {allTags.length > 0 ? (
          <div className="flex flex-wrap items-center gap-1.5">
            <button
              type="button"
              onClick={() => setSelectedTags([])}
              className={cn(
                "rounded-full border px-2.5 py-0.5 text-xs transition-colors",
                selectedTags.length === 0 ? "border-primary bg-primary text-primary-foreground" : "hover:bg-muted",
              )}
            >
              {i18next.t("general:All")}
            </button>
            {allTags.map((tag) => {
              const active = selectedTags.includes(tag);
              const color = tagColor(tag);
              return (
                <button
                  key={tag}
                  type="button"
                  onClick={() => setSelectedTags((prev) => (active ? prev.filter((t) => t !== tag) : [...prev, tag]))}
                  className={cn(
                    "rounded-full border px-2.5 py-0.5 text-xs transition-colors",
                    active ? "border-transparent font-medium" : "hover:bg-muted",
                  )}
                  style={active ? {color, backgroundColor: `color-mix(in srgb, ${color} 14%, transparent)`, borderColor: `color-mix(in srgb, ${color} 35%, transparent)`} : undefined}
                >
                  <span className="mr-1 inline-block size-1.5 rounded-full align-middle" style={{backgroundColor: color}} />
                  {tag}
                </button>
              );
            })}
          </div>
        ) : null}
      </div>

      {frequent.length > 0 ? (
        <section className="space-y-2.5">
          <h2 className="text-sm font-medium text-muted-foreground">{i18next.t("general:Frequently used")}</h2>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-6">
            {frequent.map((application: any) => (
              <AppCard
                key={application.name}
                application={application}
                variant="frequent"
                pinned={pinned.includes(application.name)}
                accountHomepage={account?.homepage ?? ""}
                onTogglePin={() => togglePin(application.name)}
                onOpen={() => openApp(application)}
              />
            ))}
          </div>
        </section>
      ) : null}

      <section className="space-y-6">
        {frequent.length > 0 && !grouped || isFiltering ? (
          <h2 className="text-sm font-medium text-muted-foreground">
            {isFiltering
              ? i18next.t("general:{{count}} applications", {count: filtered.length})
              : i18next.t("general:All")}
          </h2>
        ) : null}
        {filtered.length === 0 ? (
          isFiltering ? (
            <div className="flex flex-col items-center gap-3 rounded-lg border border-dashed py-16 text-center">
              <LayoutGrid className="size-8 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">{i18next.t("general:No matching applications")}</p>
              <Button variant="outline" size="sm" onClick={() => { setSearch(""); setSelectedTags([]); }}>
                {i18next.t("forget:Reset")}
              </Button>
            </div>
          ) : null
        ) : grouped ? (
          // category grouping: one section per tag (an app's first tag), untagged apps last
          <div className="space-y-6">
            {sections.map((section) => (
              <div key={section.tag ?? ""} className="space-y-2.5">
                <h2 className="flex items-center gap-1.5 text-sm font-medium text-muted-foreground">
                  {section.tag ? (
                    <span className="inline-block size-2 rounded-full" style={{backgroundColor: tagColor(section.tag)}} />
                  ) : null}
                  {section.tag ?? i18next.t("general:Uncategorized")}
                  <span className="text-xs text-muted-foreground/60">{section.apps.length}</span>
                </h2>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
                  {section.apps.map((application: any) => (
                    <AppCard
                      key={application.name}
                      application={application}
                      variant="full"
                      pinned={pinned.includes(application.name)}
                      highlighted={frequentNames.has(application.name)}
                      accountHomepage={account?.homepage ?? ""}
                      onTogglePin={() => togglePin(application.name)}
                      onOpen={() => openApp(application)}
                    />
                  ))}
                </div>
              </div>
            ))}
          </div>
        ) : (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
            {filtered.map((application: any) => (
              <AppCard
                key={application.name}
                application={application}
                variant="full"
                pinned={pinned.includes(application.name)}
                highlighted={frequentNames.has(application.name)}
                accountHomepage={account?.homepage ?? ""}
                onTogglePin={() => togglePin(application.name)}
                onOpen={() => openApp(application)}
              />
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

function AppLogo({application, className}: {application: any; className?: string}) {
  const [broken, setBroken] = React.useState(false);
  const name = application.displayName || application.name;
  const hasLogo = Boolean(application.logo) && !broken;
  // a padded tile keeps any logo aspect ratio looking intentional;
  // a missing or 404 logo falls back to a soft initial tile so the grid stays aligned
  return (
    <div
      className={cn(
        "flex shrink-0 items-center justify-center overflow-hidden rounded-lg",
        hasLogo ? "bg-muted/50 p-1" : "bg-muted font-semibold text-muted-foreground",
        className,
      )}
    >
      {hasLogo ? (
        <img src={application.logo} alt={name} onError={() => setBroken(true)} className="max-h-full max-w-full object-contain" />
      ) : (
        String(name).trim().charAt(0).toUpperCase()
      )}
    </div>
  );
}

function AppCard({application, variant, pinned, highlighted, accountHomepage, onTogglePin, onOpen}: {
  application: any;
  variant: "full" | "frequent";
  pinned?: boolean;
  highlighted?: boolean;
  accountHomepage: string;
  onTogglePin: () => void;
  onOpen: () => void;
}) {
  const name = application.displayName || application.name;
  const homepage = application.homepageUrl === "<custom-url>" ? accountHomepage : application.homepageUrl;
  const tags: string[] = Array.isArray(application.tags) ? application.tags : [];
  const visibleTags = variant === "frequent" ? [] : tags.slice(0, 2);

  const pinButton = (
    <button
      type="button"
      aria-label={pinned ? i18next.t("general:Unpin") : i18next.t("general:Pin to top")}
      title={pinned ? i18next.t("general:Unpin") : i18next.t("general:Pin to top")}
      onClick={onTogglePin}
      className={cn(
        "absolute right-1.5 top-1.5 z-10 rounded-md p-1 transition-all",
        pinned ? "text-primary opacity-100 hover:bg-muted" : "text-muted-foreground opacity-0 group-hover:opacity-100 hover:bg-muted group-focus-within:opacity-100",
      )}
    >
      {pinned ? <PinOff className="size-3.5" /> : <Pin className="size-3.5" />}
    </button>
  );

  // stretched link: the anchor overlays the card as a sibling of the pin
  // button, so the HTML stays valid (no interactive element inside <a>) and
  // the link is still keyboard reachable. The card opens the application
  // itself, not Casdoor's sign-in page. Apps without a homepage render as a
  // plain, non-clickable card.
  const card = (
    <Card className="group relative h-full transition-colors hover:border-foreground/20 hover:shadow-sm">
      <CardContent className={variant === "frequent" ? "flex flex-col items-center gap-2 p-3 pt-4 text-center" : "flex items-start gap-3 p-3.5"}>
        {variant === "frequent" ? (
          <>
            <AppLogo application={application} className="size-12 text-lg" />
            <div className="w-full truncate text-sm font-medium" title={name}>{name}</div>
          </>
        ) : (
          <>
            <AppLogo application={application} className="size-12 text-lg" />
            <div className="min-w-0 flex-1 pr-5">
              <div className="flex items-center gap-1.5">
                <span className="truncate font-medium" title={name}>{name}</span>
                {highlighted && !pinned ? <Pin className="size-3 shrink-0 rotate-45 text-muted-foreground/40" /> : null}
              </div>
              {application.description ? (
                <p className="mt-0.5 line-clamp-1 text-sm text-muted-foreground">{application.description}</p>
              ) : null}
              {visibleTags.length > 0 ? (
                <div className="mt-1.5 flex flex-wrap gap-1">
                  {visibleTags.map((tag) => {
                    const color = tagColor(tag);
                    return (
                      <span
                        key={tag}
                        className="rounded-full px-1.5 py-px text-[11px] leading-4"
                        style={{color, backgroundColor: `color-mix(in srgb, ${color} 12%, transparent)`}}
                      >
                        {tag}
                      </span>
                    );
                  })}
                  {tags.length > visibleTags.length ? (
                    <span className="rounded-full bg-muted px-1.5 py-px text-[11px] leading-4 text-muted-foreground">+{tags.length - visibleTags.length}</span>
                  ) : null}
                </div>
              ) : null}
            </div>
          </>
        )}
      </CardContent>
      {homepage ? (
        <a
          href={silentSigninLink(homepage)}
          target="_blank"
          rel="noreferrer"
          aria-label={name}
          className="absolute inset-0 z-0 rounded-xl focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
          onClick={onOpen}
        />
      ) : null}
      {pinButton}
    </Card>
  );

  return card;
}
