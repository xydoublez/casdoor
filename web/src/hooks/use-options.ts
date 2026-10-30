import * as React from "react";
import type {SearchableOption} from "@/components/common/SearchableSelect";
import * as AdapterBackend from "@/backend/AdapterBackend";
import * as ApplicationBackend from "@/backend/ApplicationBackend";
import * as CertBackend from "@/backend/CertBackend";
import * as GroupBackend from "@/backend/GroupBackend";
import * as ModelBackend from "@/backend/ModelBackend";
import * as OrganizationBackend from "@/backend/OrganizationBackend";
import * as PlanBackend from "@/backend/PlanBackend";
import * as ProductBackend from "@/backend/ProductBackend";
import * as PricingBackend from "@/backend/PricingBackend";
import * as ProviderBackend from "@/backend/ProviderBackend";
import * as RuleBackend from "@/backend/RuleBackend";
import * as RoleBackend from "@/backend/RoleBackend";
import * as UserBackend from "@/backend/UserBackend";

// The option lists feed comboboxes that filter client-side, so one large page is
// requested instead of paginating like the antd PaginateSelect did.
const PAGE_SIZE = 1000;

function useList<T = any>(loader: () => Promise<any> | null, deps: React.DependencyList): T[] {
  const [items, setItems] = React.useState<T[]>([]);
  const loaderRef = React.useRef(loader);
  loaderRef.current = loader;

  React.useEffect(() => {
    let cancelled = false;
    const promise = loaderRef.current();
    if (!promise) {
      setItems([]);
      return;
    }
    promise
      .then((res: any) => {
        if (!cancelled && res?.status === "ok") {
          setItems(res.data ?? []);
        }
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return items;
}

/** value: "owner/name", label: "displayName (owner/name)" — same as Setting.getDisplayNameOption. */
function toIdOptions(items: any[]): SearchableOption[] {
  return items.map((item) => {
    const id = `${item.owner}/${item.name}`;
    return {
      value: id,
      label: item.displayName ? `${item.displayName} (${id})` : id,
      keywords: `${item.name} ${item.displayName ?? ""}`,
    };
  });
}

/** value: "name" — for the fields that store a bare name inside one organization. */
function toNameOptions(items: any[]): SearchableOption[] {
  return items.map((item) => ({
    value: item.name,
    label: item.displayName ? `${item.displayName} (${item.name})` : item.name,
    keywords: `${item.name} ${item.displayName ?? ""}`,
  }));
}

export function useOrganizationOptions(): SearchableOption[] {
  const items = useList(() => OrganizationBackend.getOrganizations("admin", "", 1, PAGE_SIZE), []);
  return React.useMemo(() => toNameOptions(items), [items]);
}

/** how many backend matches a combobox search pulls per page */
const SEARCH_SIZE = 100;

export interface RemoteOptions {
  options: SearchableOption[];
  /** feed the combobox input here; "" restores the base pages */
  onSearch: (search: string) => void;
  /** scroll the combobox list to its bottom to reach everyone beyond the loaded pages */
  onLoadMore: () => void;
  hasMore: boolean;
}

function mergeUnseen(prev: any[], rows: any[]): any[] {
  const seen = new Set(prev.map((user: any) => `${user.owner}/${user.name}`));
  const merged = [...prev];
  rows.forEach((user: any) => {
    const id = `${user.owner}/${user.name}`;
    if (!seen.has(id)) {
      seen.add(id);
      merged.push(user);
    }
  });
  return merged;
}

/**
 * The users of one organization for comboboxes. A base page alone would hide
 * everyone beyond it from the search (a user like `lizhiqiang` was unfindable in
 * an organization of thousands), so the input goes to the backend name and
 * displayName filters instead — what the antd PaginateSelect used to do — and
 * scrolling the list keeps appending pages, base and search alike, so nothing
 * stays out of reach behind the first page.
 */
function useUserOptionSource(organizationName: string, map: (items: any[]) => SearchableOption[]): RemoteOptions {
  const [base, setBase] = React.useState<any[]>([]);
  const basePage = React.useRef(0);
  const [baseHasMore, setBaseHasMore] = React.useState(false);
  const baseInFlight = React.useRef(false);
  const baseTicket = React.useRef(0);

  const [matches, setMatches] = React.useState<any[] | null>(null);
  const matchPage = React.useRef(0);
  const [matchHasMore, setMatchHasMore] = React.useState(false);
  const matchInFlight = React.useRef(false);
  const seq = React.useRef(0);
  const query = React.useRef("");
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  const orgRef = React.useRef(organizationName);
  orgRef.current = organizationName;

  const loadBasePage = (page: number) => {
    const org = orgRef.current;
    if (!org || baseInFlight.current) {
      return;
    }
    const ticket = baseTicket.current;
    baseInFlight.current = true;
    UserBackend.getUsers(org, page, PAGE_SIZE)
      .then((res: any) => {
        if (ticket !== baseTicket.current) {
          return;
        }
        const rows = res?.status === "ok" ? res.data ?? [] : [];
        basePage.current = page;
        setBase((prev) => (page === 1 ? rows : mergeUnseen(prev, rows)));
        setBaseHasMore(rows.length === PAGE_SIZE);
      })
      .catch(() => undefined)
      .finally(() => {
        baseInFlight.current = false;
        if (ticket !== baseTicket.current) {
          loadBasePage(1);
        }
      });
  };

  const pendingFetch = React.useRef<{search: string; page: number; append: boolean} | null>(null);

  const runFetchMatches = (search: string, page: number, append: boolean) => {
    const org = orgRef.current;
    if (!org) {
      return;
    }
    const ticket = ++seq.current;
    matchInFlight.current = true;
    Promise.all([
      UserBackend.getUsers(org, page, SEARCH_SIZE, "name", search),
      UserBackend.getUsers(org, page, SEARCH_SIZE, "displayName", search),
    ])
      .then(([byName, byDisplayName]) => {
        if (ticket !== seq.current) {
          return;
        }
        const rows: any[] = [];
        [byName, byDisplayName].forEach((res: any) => {
          if (res?.status === "ok") {
            rows.push(...(res.data ?? []));
          }
        });
        matchPage.current = page;
        setMatches((prev) => mergeUnseen(append && prev ? prev : [], rows));
        setMatchHasMore(
          (byName?.data?.length ?? 0) === SEARCH_SIZE || (byDisplayName?.data?.length ?? 0) === SEARCH_SIZE,
        );
      })
      .catch(() => undefined)
      .finally(() => {
        matchInFlight.current = false;
        const pending = pendingFetch.current;
        pendingFetch.current = null;
        if (pending) {
          runFetchMatches(pending.search, pending.page, pending.append);
        }
      });
  };

  const fetchMatches = (search: string, page: number, append: boolean) => {
    if (matchInFlight.current) {
      // a slow request in flight must not swallow the newest query or page:
      // run it as soon as the connection frees up (latest request wins)
      pendingFetch.current = {search, page, append};
      return;
    }
    runFetchMatches(search, page, append);
  };

  React.useEffect(() => {
    baseTicket.current += 1;
    seq.current += 1;
    pendingFetch.current = null;
    setBase([]);
    setMatches(null);
    setMatchHasMore(false);
    query.current = "";
    loadBasePage(1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [organizationName]);

  React.useEffect(() => () => {
    if (timer.current) {
      clearTimeout(timer.current);
    }
  }, []);

  const onSearch = (search: string) => {
    if (timer.current) {
      clearTimeout(timer.current);
      timer.current = null;
    }
    const trimmed = search.trim();
    query.current = trimmed;
    if (!trimmed) {
      seq.current += 1;
      pendingFetch.current = null;
      setMatches(null);
      setMatchHasMore(false);
      return;
    }
    timer.current = setTimeout(() => fetchMatches(trimmed, 1, false), 300);
  };

  const onLoadMore = () => {
    if (matches !== null) {
      if (matchHasMore && query.current) {
        fetchMatches(query.current, matchPage.current + 1, true);
      }
      return;
    }
    if (baseHasMore) {
      loadBasePage(basePage.current + 1);
    }
  };

  const options = React.useMemo(() => map(matches ?? base), [matches, base, map]);
  return {options, onSearch, onLoadMore, hasMore: matches !== null ? matchHasMore : baseHasMore};
}

export function useUserOptions(organizationName: string): RemoteOptions {
  return useUserOptionSource(organizationName, toIdOptions);
}

export function useUserNameOptions(organizationName: string): RemoteOptions {
  return useUserOptionSource(organizationName, toNameOptions);
}

/**
 * The groups themselves, not just their options: the user and LDAP pages have to
 * look at each group's `type`, because only one Physical group may be picked.
 */
export function useGroupList(organizationName: string): any[] {
  return useList(
    () => (organizationName ? GroupBackend.getGroups(organizationName, false, 1, PAGE_SIZE) : null),
    [organizationName],
  );
}

export function useGroupOptions(organizationName: string): SearchableOption[] {
  const items = useGroupList(organizationName);
  return React.useMemo(() => toIdOptions(items), [items]);
}

export function useGroupNameOptions(organizationName: string): SearchableOption[] {
  const items = useGroupList(organizationName);
  return React.useMemo(() => toNameOptions(items), [items]);
}

/**
 * Keeps at most one Physical group in a selection, the rule both the antd user
 * page and LDAP page enforce. Returns null when the selection is fine as it is.
 */
export function dropExtraPhysicalGroups(values: string[], groups: any[]): string[] | null {
  const isPhysical = (value: string) =>
    groups.find((group: any) => `${group.owner}/${group.name}` === value)?.type === "Physical";
  const physical = values.filter(isPhysical);
  if (physical.length <= 1) {
    return null;
  }
  return values.filter((value) => !isPhysical(value) || value === physical[0]);
}

export function useRoleOptions(organizationName: string, exclude?: string): SearchableOption[] {
  const items = useList(() => (organizationName ? RoleBackend.getRoles(organizationName, 1, PAGE_SIZE) : null), [organizationName]);
  return React.useMemo(() => toIdOptions(items).filter((option) => option.value !== exclude), [items, exclude]);
}

export function useRoleNameOptions(organizationName: string): SearchableOption[] {
  const items = useList(() => (organizationName ? RoleBackend.getRoles(organizationName, 1, PAGE_SIZE) : null), [organizationName]);
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function useModelOptions(organizationName: string): SearchableOption[] {
  const items = useList(() => (organizationName ? ModelBackend.getModels(organizationName, 1, PAGE_SIZE) : null), [organizationName]);
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function useAdapterOptions(organizationName: string): SearchableOption[] {
  const items = useList(
    () => (organizationName ? AdapterBackend.getAdapters(organizationName, 1, PAGE_SIZE) : null),
    [organizationName],
  );
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function useApplicationOptions(organizationName: string): SearchableOption[] {
  const items = useList(
    () => (organizationName ? ApplicationBackend.getApplicationsByOrganization("admin", organizationName, 1, PAGE_SIZE) : null),
    [organizationName],
  );
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function useProviderOptions(organizationName: string, category?: string): SearchableOption[] {
  const items = useList(
    () => (organizationName ? ProviderBackend.getProviders(organizationName, 1, PAGE_SIZE) : null),
    [organizationName],
  );
  return React.useMemo(
    () => toNameOptions(category ? items.filter((item: any) => item.category === category) : items),
    [items, category],
  );
}

/**
 * The provider objects themselves, not just their names — the application's
 * provider table decides which columns apply from each one's category and type.
 */
export function useProviderList(organizationName: string): any[] {
  return useList(
    () => (organizationName ? ProviderBackend.getProviders(organizationName, 1, PAGE_SIZE) : null),
    [organizationName],
  );
}

export function useCertOptions(organizationName: string, scope?: string): SearchableOption[] {
  const items = useList(() => (organizationName ? CertBackend.getCerts(organizationName, 1, PAGE_SIZE) : null), [organizationName]);
  return React.useMemo(
    () => toNameOptions(scope ? items.filter((item: any) => item.scope === scope) : items),
    [items, scope],
  );
}

export function usePlanOptions(organizationName: string): SearchableOption[] {
  const items = useList(() => (organizationName ? PlanBackend.getPlans(organizationName, 1, PAGE_SIZE) : null), [organizationName]);
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function useProductOptions(organizationName: string): SearchableOption[] {
  const items = useList(
    () => (organizationName ? ProductBackend.getProducts(organizationName, 1, PAGE_SIZE) : null),
    [organizationName],
  );
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function usePricingOptions(organizationName: string): SearchableOption[] {
  const items = useList(
    () => (organizationName ? PricingBackend.getPricings(organizationName, 1, PAGE_SIZE) : null),
    [organizationName],
  );
  return React.useMemo(() => toNameOptions(items), [items]);
}

export function useRuleOptions(organizationName: string): SearchableOption[] {
  // the rules API paginates but takes no filter arguments
  const items = useList(() => (organizationName ? RuleBackend.getRules(organizationName, 1, PAGE_SIZE) : null), [organizationName]);
  return React.useMemo(() => toNameOptions(items), [items]);
}
