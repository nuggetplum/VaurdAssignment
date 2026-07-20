"""Minimal Streamlit dashboard for the Vaurd order-processing service.

Two views, switched via the sidebar:
  - Orders Table: GET /orders with status/sort/limit/offset controls.
  - Health Check: GET /healthz with a green/red status indicator.

No auth, no styling libraries -- just requests + streamlit, reading the
backend's base URL from the API_URL env var.
"""

import os

import requests
import streamlit as st

API_URL = os.getenv("API_URL", "http://localhost:8080")

STATUS_OPTIONS = ["(any)", "Received", "Preparing", "Complete", "Cancelled"]
SORT_OPTIONS = ["last_updated_desc", "last_updated_asc"]

st.set_page_config(page_title="Vaurd Order Dashboard", layout="wide")
st.sidebar.title("Vaurd Order Dashboard")
st.sidebar.caption(f"API: {API_URL}")
page = st.sidebar.radio("Page", ["Orders Table", "Health Check"])


def render_orders_table():
    st.title("Orders Table")

    col1, col2, col3, col4, col5 = st.columns([2, 2, 1, 1, 1])
    with col1:
        status = st.selectbox("Status", STATUS_OPTIONS)
    with col2:
        sort = st.selectbox("Sort", SORT_OPTIONS)
    with col3:
        limit = st.number_input("Limit", min_value=1, max_value=200, value=50, step=1)
    with col4:
        offset = st.number_input("Offset", min_value=0, value=0, step=1)
    with col5:
        st.write("")  # vertical spacer so the button lines up with the inputs
        st.button("Refresh")  # any click reruns the whole script, refetching below

    params = {"sort": sort, "limit": int(limit), "offset": int(offset)}
    if status != "(any)":
        params["status"] = status

    with st.spinner("Loading orders..."):
        try:
            resp = requests.get(f"{API_URL}/orders", params=params, timeout=5)
        except requests.exceptions.RequestException as err:
            st.error(f"Could not reach the API at {API_URL}: {err}")
            return

    if resp.status_code == 400:
        st.error(f"Bad request (400): {resp.text}")
        return
    if resp.status_code != 200:
        st.error(f"Unexpected response ({resp.status_code}): {resp.text}")
        return

    data = resp.json()
    orders = data.get("orders", [])
    total = data.get("total", 0)

    st.caption(
        f"Showing {len(orders)} of {total} total orders "
        f"(limit={data.get('limit')}, offset={data.get('offset')})"
    )

    if not orders:
        st.info("No orders found for this filter.")
        return

    rows = []
    for o in orders:
        items = ", ".join(f"{i['itemId']} x{i['qty']}" for i in o.get("items", []))
        rows.append(
            {
                "Order ID": o.get("orderId"),
                "Customer": o.get("customerId") or "(unknown)",
                "Restaurant": o.get("restaurantId") or "(unknown)",
                "Status": o.get("status") or "(unknown)",
                "Items": items or "(none)",
                "Last Updated": o.get("lastUpdated"),
            }
        )

    st.dataframe(rows, width="stretch", hide_index=True)


def render_health_check():
    st.title("Health Check")

    with st.spinner("Checking..."):
        try:
            resp = requests.get(f"{API_URL}/healthz", timeout=5)
        except requests.exceptions.RequestException as err:
            st.markdown("## 🔴 DOWN")
            st.error(f"Could not reach the API at {API_URL}: {err}")
            return

    if resp.status_code == 200:
        st.markdown("## 🟢 HEALTHY")
        st.caption(f"GET {API_URL}/healthz -> 200 OK")
    else:
        st.markdown("## 🔴 UNHEALTHY")
        st.error(f"GET {API_URL}/healthz -> {resp.status_code}: {resp.text}")


if page == "Orders Table":
    render_orders_table()
else:
    render_health_check()
