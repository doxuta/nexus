-- Roll back 000042: drop the app-container tables, child (nav items) before parent.
DROP TABLE IF EXISTS sys_app_nav_items;
DROP TABLE IF EXISTS sys_apps;
